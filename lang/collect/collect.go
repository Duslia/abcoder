// Copyright 2025 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collect

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/cloudwego/abcoder/lang/cxx"
	"github.com/cloudwego/abcoder/lang/java"
	"github.com/cloudwego/abcoder/lang/java/parser"
	"github.com/cloudwego/abcoder/lang/log"
	. "github.com/cloudwego/abcoder/lang/lsp"
	"github.com/cloudwego/abcoder/lang/python"
	"github.com/cloudwego/abcoder/lang/rust"
	"github.com/cloudwego/abcoder/lang/uniast"
)

type CollectOption struct {
	Language           uniast.Language
	LoadExternalSymbol bool
	NeedStdSymbol      bool
	NoNeedComment      bool
	NotNeedTest        bool
	Excludes           []string
	LoadByPackages     bool
	NeedConcurrent     bool
	// 并发配置参数
	MaxWorkers    int  // 最大工作协程数
	EnableSorting bool // 是否启用文件大小排序
	BufferSize    int  // 通道缓冲区大小
}

type Collector struct {
	cli  *LSPClient
	spec LanguageSpec

	repo string

	// 🆕 新增：并发处理相关字段
	clientPool *LSPClientPool
	mu         sync.RWMutex // 保护共享状态

	// 并发配置参数
	maxWorkers    int  // 最大工作协程数
	enableSorting bool // 是否启用文件大小排序
	bufferSize    int  // 通道缓冲区大小

	syms map[Location]*DocumentSymbol

	//  symbol => (receiver,impl,func)
	funcs map[*DocumentSymbol]functionInfo

	// 	symbol => [deps]
	deps map[*DocumentSymbol][]dependency

	// variable (or const) => type
	vars map[*DocumentSymbol]dependency

	files map[string]*uniast.File

	localLSPSymbol map[DocumentURI]map[Range]*DocumentSymbol

	localFunc map[Location]*DocumentSymbol

	// modPatcher ModulePatcher

	CollectOption
}

type methodInfo struct {
	Receiver  dependency  `json:"receiver"`
	Interface *dependency `json:"implement,omitempty"` // which interface it implements
	ImplHead  string      `json:"implHead,omitempty"`
}

type functionInfo struct {
	Method           *methodInfo        `json:"method,omitempty"`
	TypeParams       map[int]dependency `json:"typeParams,omitempty"`
	TypeParamsSorted []dependency       `json:"-"`
	Inputs           map[int]dependency `json:"inputs,omitempty"`
	InputsSorted     []dependency       `json:"-"`
	Outputs          map[int]dependency `json:"outputs,omitempty"`
	OutputsSorted    []dependency       `json:"-"`
	Signature        string             `json:"signature,omitempty"`
}

func switchSpec(l uniast.Language, repo string) LanguageSpec {
	switch l {
	case uniast.Rust:
		return rust.NewRustSpec()
	case uniast.Cxx:
		return cxx.NewCxxSpec()
	case uniast.Python:
		return python.NewPythonSpec()
	case uniast.Java:
		return java.NewJavaSpec(repo)
	default:
		panic(fmt.Sprintf("unsupported language %s", l))
	}
}

func NewCollector(repo string, cli *LSPClient) *Collector {
	ret := &Collector{
		repo:  repo,
		cli:   cli,
		spec:  switchSpec(cli.ClientOptions.Language, repo),
		syms:  map[Location]*DocumentSymbol{},
		funcs: map[*DocumentSymbol]functionInfo{},
		deps:  map[*DocumentSymbol][]dependency{},
		vars:  map[*DocumentSymbol]dependency{},
		files: map[string]*uniast.File{},
	}
	// if cli.Language == uniast.Rust {
	// 	ret.modPatcher = &rust.RustModulePatcher{Root: repo}
	// }
	return ret
}

// 🆕 新增：使用客户端池的构造函数
func NewCollectorWithPool(repo string, pool *LSPClientPool) *Collector {
	// 从池中获取一个客户端作为主客户端
	if pool == nil {
		panic("failed to acquire LSP client from pool")
	}
	cli := pool.Acquire()

	ret := &Collector{
		repo:       repo,
		cli:        cli,
		clientPool: pool,
		spec:       switchSpec(cli.ClientOptions.Language, repo),

		// 设置默认并发配置
		maxWorkers:    4,    // 默认4个工作协程
		enableSorting: true, // 默认启用文件大小排序
		bufferSize:    10,   // 默认缓冲区大小为10

		syms:  map[Location]*DocumentSymbol{},
		funcs: map[*DocumentSymbol]functionInfo{},
		deps:  map[*DocumentSymbol][]dependency{},
		vars:  map[*DocumentSymbol]dependency{},
		files: map[string]*uniast.File{},
	}
	pool.Release(cli)
	return ret
}

// 🆕 新增：使用多个客户端的构造函数
func NewCollectorWithMultipleClients(repo string, clients []*LSPClient) *Collector {
	if len(clients) == 0 {
		panic("at least one LSP client is required")
	}

	// 使用现有的 NewLSPClientPool 函数创建池
	pool := NewLSPClientPool(clients)

	return NewCollectorWithPool(repo, pool)
}

// SetConcurrencyConfig 设置并发配置参数
func (c *Collector) SetConcurrencyConfig(maxWorkers int, enableSorting bool, bufferSize int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if maxWorkers > 0 {
		c.maxWorkers = maxWorkers
	}
	c.enableSorting = enableSorting
	if bufferSize > 0 {
		c.bufferSize = bufferSize
	}
}

// GetConcurrencyConfig 获取当前并发配置参数
func (c *Collector) GetConcurrencyConfig() (maxWorkers int, enableSorting bool, bufferSize int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.maxWorkers, c.enableSorting, c.bufferSize
}

func (c *Collector) configureLSP(ctx context.Context) {
	// XXX: should be put in language specification
	if c.Language == uniast.Python {
		if !c.NeedStdSymbol {
			if c.Language == uniast.Python {
				conf := map[string]interface{}{
					"settings": map[string]interface{}{
						"pylsp": map[string]interface{}{
							"plugins": map[string]interface{}{
								"jedi_definition": map[string]interface{}{
									"follow_builtin_definitions": false,
								},
							},
						},
					},
				}
				c.cli.Notify(ctx, "workspace/didChangeConfiguration", conf)
			}
		}
	}
}

func (c *Collector) Collect(ctx context.Context) error {
	var root_syms []*DocumentSymbol
	var err error
	if c.Language == uniast.Java {
		root_syms, err = c.ScannerByTreeSitter(ctx)
		if err != nil {
			return err
		}
	} else {
		root_syms = c.ScannerFile(ctx)
	}

	// collect some extra metadata
	entity_syms := make([]*DocumentSymbol, 0, len(root_syms))
	for _, sym := range root_syms {
		// only language entity symbols need to be collect on next
		if c.spec.IsEntitySymbol(*sym) {
			entity_syms = append(entity_syms, sym)
		}
		if c.Language != uniast.Java {
			c.processSymbol(ctx, sym, 1)
		}
	}

	// collect internal references
	// for _, sym := range syms {
	// 	i := c.spec.DeclareTokenOfSymbol(*sym)
	// 	if i < 0 {
	// 		log.Error("declare token of symbol %s failed\n", sym)
	// 		continue
	// 	}
	// 	refs, err := c.cli.References(ctx, sym.Tokens[i].Location)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	for _, rloc := range refs {
	// 		// remove child symbol
	// 		if sym.Location.Include(rloc) {
	// 			continue
	// 		}
	// 		rsym, err := c.getSymbolByLocation(ctx, rloc)
	// 		if err != nil || rsym == nil {
	// 			log.Error("symbol not found for location %v\n", rloc)
	// 			continue
	// 		}
	// 		// remove external or parent symbol
	// 		if !c.internal(rsym.Location) || rsym.Location.Include(sym.Location) {
	// 			continue
	// 		}
	// 		c.deps[rsym] = append(c.deps[rsym], Dependency{
	// 			Location: rloc,
	// 			Symbol:   sym,
	// 		})
	// 	}
	// }

	// collect dependencies
	for _, sym := range entity_syms {
	next_token:

		for i, token := range sym.Tokens {
			// only entity token need to be collect (std token is only collected when NeedStdSymbol is true)
			if !c.spec.IsEntityToken(token) {
				continue
			}

			// skip function's params
			if sym.Kind == SKFunction || sym.Kind == SKMethod {
				if finfo, ok := c.funcs[sym]; ok {
					if finfo.Method != nil {
						if finfo.Method.Receiver.Location.Include(token.Location) {
							continue next_token
						}
					}
					if finfo.Inputs != nil {
						if _, ok := finfo.Inputs[i]; ok {
							continue next_token
						}
					}
					if finfo.Outputs != nil {
						if _, ok := finfo.Outputs[i]; ok {
							continue next_token
						}
					}
					if finfo.TypeParams != nil {
						if _, ok := finfo.TypeParams[i]; ok {
							continue next_token
						}
					}
				}
			}
			// skip variable's type
			if sym.Kind == SKVariable || sym.Kind == SKConstant {
				if dep, ok := c.vars[sym]; ok {
					if dep.Location.Include(token.Location) {
						continue next_token
					}
				}
			}

			// go to definition
			dep, err := c.getSymbolByToken(ctx, token)
			if err != nil || dep == nil {
				if token.Type == "method_invocation" || token.Type == "static_method_invocation" {
					// 外部依赖无法从LSP 中查询到定义，先不报错
					continue
				}
				log.Error("dep token %v not found: %v\n", token, err)
				continue
			}

			// NOTICE: some internal symbols may not been get by DocumentSymbols, thus we let Unknown symbol pass
			if dep.Kind == SKUnknown && c.internal(dep.Location) {
				// try get symbol kind by token
				sk := c.spec.TokenKind(token)
				if sk != SKUnknown {
					dep.Kind = sk
					dep.Name = token.Text
				}
			}

			// remove local symbols
			if sym.Location.Include(dep.Location) {
				continue
			} else {
				c.syms[dep.Location] = dep
			}

			c.deps[sym] = append(c.deps[sym], dependency{
				Location: token.Location,
				Symbol:   dep,
			})

		}
	}

	return nil
}

func (c *Collector) ScannerFile(ctx context.Context) []*DocumentSymbol {
	c.configureLSP(ctx)
	excludes := make([]string, len(c.Excludes))
	for i, e := range c.Excludes {
		if !filepath.IsAbs(e) {
			excludes[i] = filepath.Join(c.repo, e)
		} else {
			excludes[i] = e
		}
	}

	// scan all files
	root_syms := make([]*DocumentSymbol, 0, 1024)
	scanner := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		for _, e := range excludes {
			if strings.HasPrefix(path, e) {
				return nil
			}
		}

		if c.spec.ShouldSkip(path) {
			return nil
		}

		file := c.files[path]
		if file == nil {
			rel, err := filepath.Rel(c.repo, path)
			if err != nil {
				return err
			}
			file = uniast.NewFile(rel)
			c.files[path] = file
		}

		// 解析use语句
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		uses, err := c.spec.FileImports(content)
		if err != nil {
			log.Error("parse file %s use statements failed: %v", path, err)
		} else {
			file.Imports = uses
		}

		// collect symbols
		uri := NewURI(path)
		symbols, err := c.cli.DocumentSymbols(ctx, uri)
		if err != nil {
			return err
		}
		// file := filepath.Base(path)
		for _, sym := range symbols {
			// collect content
			content, err := c.cli.Locate(sym.Location)
			if err != nil {
				return err
			}
			// collect tokens
			tokens, err := c.cli.SemanticTokens(ctx, sym.Location)
			if err != nil {
				return err
			}
			sym.Text = content
			sym.Tokens = tokens
			c.syms[sym.Location] = sym
			root_syms = append(root_syms, sym)
		}

		return nil
	}
	if err := filepath.Walk(c.repo, scanner); err != nil {
		log.Error("scan files failed: %v", err)
	}
	return root_syms
}

func (c *Collector) ScannerByTreeSitter(ctx context.Context) ([]*DocumentSymbol, error) {
	// 如果有客户端池，使用并发处理
	if c.clientPool != nil {
		return c.scannerByTreeSitterConcurrent(ctx)
	}

	// 否则使用原有的单线程处理
	return c.scannerByTreeSitterSequential(ctx)
}

// 原有的单线程处理方法
func (c *Collector) scannerByTreeSitterSequential(ctx context.Context) ([]*DocumentSymbol, error) {
	var modulePaths []string
	// Java uses parsing pom method to obtain hierarchical relationships
	if c.Language == uniast.Java {
		rootPomPath := filepath.Join(c.repo, "pom.xml")
		rootModule, err := parser.ParseMavenProject(rootPomPath)
		if err != nil {
			// 尝试直接遍历文件
			modulePaths = append(modulePaths, c.repo)
		} else {
			modulePaths = parser.GetModulePaths(rootModule)
		}
		// Collect all module paths from the maven project structure
	}

	c.configureLSP(ctx)
	excludes := make([]string, len(c.Excludes))
	for i, e := range c.Excludes {
		if !filepath.IsAbs(e) {
			excludes[i] = filepath.Join(c.repo, e)
		} else {
			excludes[i] = e
		}
	}

	totalStartTime := time.Now()
	fileList := make([]string, 0)

	for _, modulePath := range modulePaths {
		err := filepath.Walk(modulePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			// 检查排除路径
			for _, e := range excludes {
				if strings.HasPrefix(path, e) {
					return nil
				}
			}
			if c.spec.ShouldSkip(path) {
				return nil
			}
			uri := NewURI(path)
			_, err = c.cli.DidOpen(ctx, uri)
			if err != nil {
				return err
			}
			fileList = append(fileList, path)
			return nil
		})
		if err != nil {
			log.Error("scan files failed: %v", err)
		}
	}
	OpenFilesince := time.Since(totalStartTime)

	searchStart := time.Now()
	r, err := c.cli.WorkspaceSearchSymbols(ctx, "String")
	since := time.Since(searchStart)
	Toaltsince := time.Since(totalStartTime)

	log.Info("search symbols took %v, search time %v, total %v ,nums %D", OpenFilesince, since, Toaltsince, len(r))
	if err != nil {
		return nil, err
	}

	scanner := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		for _, e := range excludes {
			if strings.HasPrefix(path, e) {
				return nil
			}
		}

		if c.spec.ShouldSkip(path) {
			return nil
		}

		file := c.files[path]
		if file == nil {
			rel, err := filepath.Rel(c.repo, path)
			if err != nil {
				return err
			}
			file = uniast.NewFile(rel)
			c.files[path] = file
		}

		// 解析use语句
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		uri := NewURI(path)
		_, err = c.cli.DidOpen(ctx, uri)
		if err != nil {
			return err
		}
		tree, err := parser.Parse(ctx, c.cli.P, content)
		if err != nil {
			log.Error("parse file %s failed: %v", path, err)
			return nil // continue with next file
		}

		uri = NewURI(path)
		c.walk(tree.RootNode(), uri, content, file, nil)

		return nil
	}

	// Walk each module path to find and parse files in module
	for i, modulePath := range modulePaths {
		if err := filepath.Walk(modulePath, scanner); err != nil {
			log.Error("scan files failed: %v", err)
		}
		log.Info("finish collector module %v ，progress rate %d/%d ", modulePath, i, len(modulePaths))
	}

	root_syms := make([]*DocumentSymbol, 0, 1024)

	for _, symbol := range c.syms {
		root_syms = append(root_syms, symbol)
	}
	return root_syms, nil
}

// 新的并发处理方法
func (c *Collector) scannerByTreeSitterConcurrent(ctx context.Context) ([]*DocumentSymbol, error) {
	var modulePaths []string
	// Java uses parsing pom method to obtain hierarchical relationships
	if c.Language == uniast.Java {
		rootPomPath := filepath.Join(c.repo, "pom.xml")
		rootModule, err := parser.ParseMavenProject(rootPomPath)
		if err != nil {
			// 尝试直接遍历文件
			modulePaths = append(modulePaths, c.repo)
		} else {
			modulePaths = parser.GetModulePaths(rootModule)
		}
	}

	// 配置排除路径
	excludes := make([]string, len(c.Excludes))
	for i, e := range c.Excludes {
		if !filepath.IsAbs(e) {
			excludes[i] = filepath.Join(c.repo, e)
		} else {
			excludes[i] = e
		}
	}

	// 收集所有需要处理的文件
	var allFiles []fileInfo

	for _, modulePath := range modulePaths {
		err := filepath.Walk(modulePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			// 检查排除路径
			for _, e := range excludes {
				if strings.HasPrefix(path, e) {
					return nil
				}
			}
			if c.spec.ShouldSkip(path) {
				return nil
			}
			allFiles = append(allFiles, fileInfo{
				path: path,
				fi:   info,
				size: info.Size(),
			})
			return nil
		})
		if err != nil {
			log.Error("scan files failed: %v", err)
		}
	}

	// 按文件大小排序（大文件优先处理，提高并发效率）
	c.sortFilesBySize(allFiles)

	//// 使用基于文件大小的负载均衡分配文件
	workerFiles := c.distributeFilesBySize(allFiles, c.maxWorkers)

	// 并发处理文件
	return c.processFilesConcurrently(ctx, workerFiles)
}

// processFilesConcurrently 并发处理文件列表
func (c *Collector) processFilesConcurrently(ctx context.Context, fileTaskList [][]fileInfo) ([]*DocumentSymbol, error) {
	if len(fileTaskList) == 0 {
		return nil, nil
	}
	metrics := PerformanceMetrics{}

	totalStartTime := time.Now()

	// 确定工作协程数量
	maxWorkers := c.maxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 4 // 默认值
	}
	// 不能超过客户端池大小
	if c.clientPool != nil && maxWorkers > c.clientPool.Size() {
		maxWorkers = c.clientPool.Size()
	}
	// 不能超过文件数量
	if maxWorkers > len(fileTaskList) {
		maxWorkers = len(fileTaskList)
	}

	log.Info("Starting concurrent processing with %d workers", maxWorkers)

	// 创建结果收集器
	results := make([][]*tempCollectorResult, maxWorkers)
	var wg sync.WaitGroup

	// 启动worker协程，每个协程处理分配给它的文件列表
	for workerID := 0; workerID < maxWorkers; workerID++ {
		if len(fileTaskList[workerID]) == 0 {
			continue // 跳过没有分配文件的worker
		}

		wg.Add(1)
		go func(workerID int, fileList []fileInfo) {
			defer wg.Done()
			workerMetrics := WorkerMetrics{}

			workerStartTime := time.Now()

			// 获取LSP客户端（每个worker只获取一次）
			clientAcquireStart := time.Now()
			client := c.clientPool.Acquire()
			if client == nil {
				log.Error("Worker %d failed to acquire client: pool is closed", workerID)
				return
			}
			defer c.clientPool.Release(client)
			clientAcquireTime := time.Since(clientAcquireStart)
			workerMetrics.WorkerID = workerID
			workerMetrics.FileCount = len(fileList)
			workerMetrics.ClientAcquireTime = clientAcquireTime
			workerMetrics.ClientAcquireTime = clientAcquireTime

			log.Info("Worker %d acquired client in %v, processing %d files", workerID, clientAcquireTime, len(fileList))

			// 创建临时收集器
			tempCollector := &Collector{
				cli:            client,
				spec:           c.spec,
				repo:           c.repo,
				syms:           make(map[Location]*DocumentSymbol),
				funcs:          make(map[*DocumentSymbol]functionInfo),
				deps:           make(map[*DocumentSymbol][]dependency),
				vars:           make(map[*DocumentSymbol]dependency),
				files:          make(map[string]*uniast.File),
				localLSPSymbol: make(map[DocumentURI]map[Range]*DocumentSymbol),
				localFunc:      make(map[Location]*DocumentSymbol),
				CollectOption:  c.CollectOption,
			}
			// 处理分配给该worker的所有文件
			workerResults := make([]*tempCollectorResult, 0, len(fileList))
			for _, file := range fileList {

				if ctx.Err() != nil {
					log.Error("Worker %d stopped due to context cancellation", workerID)
					return
				}
				fileMetrics := FileMetrics{}
				result := c.processFile(ctx, tempCollector, &fileMetrics, client, file)
				if result != nil {
					workerResults = append(workerResults, result)
				}
				workerMetrics.FileMetrics = append(workerMetrics.FileMetrics, fileMetrics)
			}
			// 更新worker指标
			workerTotalTime := time.Since(workerStartTime)
			workerMetrics.TotalProcessTime = workerTotalTime
			metrics.WorkerMetrics = append(metrics.WorkerMetrics, workerMetrics)
			log.Info("Worker %d completed processing %d files in %v (client acquire: %v)",
				workerID, len(fileList), workerTotalTime, clientAcquireTime)
			// 保存结果
			results[workerID] = workerResults
		}(workerID, fileTaskList[workerID])
	}

	// 等待所有worker完成
	wg.Wait()

	// 合并所有结果
	mergeStartTime := time.Now()
	var allResults []*tempCollectorResult
	for _, workerResults := range results {
		allResults = append(allResults, workerResults...)
	}

	finalSymbols := c.mergeResults(allResults)
	mergeTime := time.Since(mergeStartTime)

	// 更新总体性能指标
	totalTime := time.Since(totalStartTime)
	metrics.TotalProcessTime = totalTime
	metrics.MergeTime = mergeTime

	log.Info("Concurrent processing completed in %v (load balancing: %v, merge: %v)",
		totalTime, metrics.LoadBalancingTime, mergeTime)

	return finalSymbols, nil
}

// tempCollectorResult 临时收集器结果
// fileInfo 文件信息，用于排序
type fileInfo struct {
	path string
	size int64
	fi   os.FileInfo
}

type tempCollectorResult struct {
	symbols map[Location]*DocumentSymbol
	files   map[string]*uniast.File
	funcs   map[*DocumentSymbol]functionInfo
	deps    map[*DocumentSymbol][]dependency
	vars    map[*DocumentSymbol]dependency
}

// PerformanceMetrics 性能监控指标
type PerformanceMetrics struct {
	FileCollectionTime time.Duration   // 文件收集耗时
	LoadBalancingTime  time.Duration   // 负载均衡分配耗时
	TotalProcessTime   time.Duration   // 总处理时间
	WorkerMetrics      []WorkerMetrics // 每个worker的指标
	MergeTime          time.Duration   // 结果合并耗时
}

// WorkerMetrics 单个worker的性能指标
type WorkerMetrics struct {
	WorkerID          int           // Worker ID
	ClientAcquireTime time.Duration // 客户端获取耗时
	TotalProcessTime  time.Duration // 总处理时间
	FileCount         int           // 处理的文件数量
	TotalFileSize     int64         // 处理的文件总大小
	FileMetrics       []FileMetrics // 每个文件的处理指标
}

// FileMetrics 单个文件的处理指标
type FileMetrics struct {
	FilePath         string        // 文件路径
	FileSize         int64         // 文件大小
	ProcessTime      time.Duration // 处理耗时
	LSPTime          time.Duration // LSP操作耗时
	ParseTime        time.Duration // 解析耗时
	WalkTime         time.Duration // AST遍历耗时
	TotalProcessTime time.Duration // 总共耗时
}

// processFile 处理单个文件
func (c *Collector) processFile(ctx context.Context, tempCollector *Collector, fm *FileMetrics, client *LSPClient, fileInfo fileInfo) *tempCollectorResult {
	fileStartTime := time.Now()

	// 处理文件
	file := tempCollector.files[fileInfo.path]
	if file == nil {
		rel, err := filepath.Rel(c.repo, fileInfo.path)
		if err != nil {
			log.Error("failed to get relative path for %s: %v", fileInfo.path, err)
			return nil
		}
		file = uniast.NewFile(rel)
		tempCollector.files[fileInfo.path] = file
	}

	// 读取文件内容
	readStartTime := time.Now()
	// 解析use语句
	content, err := os.ReadFile(fileInfo.path)
	if err != nil {
		return nil
	}
	readTime := time.Since(readStartTime)

	// LSP操作：打开文件
	lspStartTime := time.Now()
	uri := NewURI(fileInfo.path)
	_, err = client.DidOpen(ctx, uri)
	if err != nil {
		log.Error("failed to open file %s: %v", fileInfo.path, err)
		return nil
	}
	lspTime := time.Since(lspStartTime)

	// 解析文件
	parseStartTime := time.Now()
	tree, err := parser.Parse(ctx, client.P, content)
	if err != nil {
		log.Error("parse file %s failed: %v", fileInfo.path, err)
		return nil
	}
	parseTime := time.Since(parseStartTime)

	// 遍历AST
	walkStartTime := time.Now()
	tempCollector.walk(tree.RootNode(), uri, content, file, nil)
	walkTime := time.Since(walkStartTime)

	// 计算总处理时间
	totalProcessTime := time.Since(fileStartTime)

	// 记录详细的性能指标（可选：仅在调试模式下记录）
	if c.CollectOption.Language != "" { // 简单的条件判断，避免过多日志
		log.Info("File %s processed in %v (read: %v, lsp: %v, parse: %v, walk: %v, size: %d bytes)",
			filepath.Base(fileInfo.path), totalProcessTime, readTime, lspTime, parseTime, walkTime, fileInfo.size)
	}

	fm.FilePath = fileInfo.path
	fm.FileSize = fileInfo.size
	fm.ProcessTime = parseTime
	fm.LSPTime = lspTime
	fm.ParseTime = parseTime
	fm.WalkTime = walkTime
	fm.TotalProcessTime = totalProcessTime

	// 返回结果
	return &tempCollectorResult{
		symbols: tempCollector.syms,
		files:   tempCollector.files,
		funcs:   tempCollector.funcs,
		deps:    tempCollector.deps,
		vars:    tempCollector.vars,
	}
}

// mergeResults 合并多个临时收集器的结果
func (c *Collector) mergeResults(results []*tempCollectorResult) []*DocumentSymbol {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 统计信息
	var totalSymbols, totalFiles, totalFuncs, totalDeps, totalVars int

	// 合并所有结果到主收集器
	for _, result := range results {
		if result == nil {
			continue
		}

		// 合并符号（检查冲突）
		for loc, sym := range result.symbols {
			if existing, exists := c.syms[loc]; exists {
				// 如果存在冲突，保留更详细的符号信息
				if c.isMoreDetailed(sym, existing) {
					c.syms[loc] = sym
				}
			} else {
				c.syms[loc] = sym
			}
			totalSymbols++
		}

		// 合并文件
		for path, file := range result.files {
			c.files[path] = file
			totalFiles++
		}

		// 合并函数信息
		for sym, info := range result.funcs {
			c.funcs[sym] = info
			totalFuncs++
		}

		// 合并依赖
		for sym, deps := range result.deps {
			c.deps[sym] = deps
			totalDeps++
		}

		// 合并变量
		for sym, dep := range result.vars {
			c.vars[sym] = dep
			totalVars++
		}
	}

	// 记录合并统计信息
	log.Info("Merged results: %d symbols, %d files, %d functions, %d dependencies, %d variables",
		totalSymbols, totalFiles, totalFuncs, totalDeps, totalVars)

	// 返回所有符号
	root_syms := make([]*DocumentSymbol, 0, len(c.syms))
	for _, symbol := range c.syms {
		root_syms = append(root_syms, symbol)
	}

	return root_syms
}

// isMoreDetailed 判断哪个符号包含更多详细信息
func (c *Collector) isMoreDetailed(sym1, sym2 *DocumentSymbol) bool {
	// 比较符号的详细程度
	score1 := c.calculateSymbolScore(sym1)
	score2 := c.calculateSymbolScore(sym2)
	return score1 > score2
}

// calculateSymbolScore 计算符号的详细程度分数
func (c *Collector) calculateSymbolScore(sym *DocumentSymbol) int {
	if sym == nil {
		return 0
	}

	score := 0

	// 基础分数
	score += 1

	// 如果有文本信息，加分
	if sym.Text != "" {
		score += 2
	}

	// 如果有子符号，加分
	if len(sym.Children) > 0 {
		score += len(sym.Children)
	}

	// 如果有范围信息，加分
	if sym.Location.Range.Start.Line != sym.Location.Range.End.Line ||
		sym.Location.Range.Start.Character != sym.Location.Range.End.Character {
		score += 1
	}

	// 如果有Token信息，加分
	if len(sym.Tokens) > 0 {
		score += 1
	}

	return score
}

// sortFilesBySize 按文件大小排序（大文件优先）
func (c *Collector) sortFilesBySize(files []fileInfo) {
	// 使用简单的冒泡排序，按文件大小降序排列
	n := len(files)
	for i := 0; i < n-1; i++ {
		for j := 0; j < n-i-1; j++ {
			if files[j].size < files[j+1].size {
				files[j], files[j+1] = files[j+1], files[j]
			}
		}
	}
}

// distributeFilesBySize 基于文件大小进行负载均衡分配
// 使用贪心算法将文件分配给不同的worker，尽量平衡每个worker的总工作量
func (c *Collector) distributeFilesBySize(files []fileInfo, maxWorkers int) [][]fileInfo {

	// 初始化worker列表
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if maxWorkers > len(files) {
		maxWorkers = len(files)
	}
	rund := len(files) / maxWorkers
	count := 0
	workerFiles := make([][]fileInfo, maxWorkers)
	// 已经排序好了，就按照当前每轮最前面的就是相对较大的，每次都分配给当前轮次最大的worker
	for i := 0; i < rund && count < len(files); i++ {
		for j := 0; j < maxWorkers; j++ {
			workerFiles[j] = append(workerFiles[j], files[i*maxWorkers+j])
			count++
		}
	}

	for i := count; i < len(files); i++ {
		workerFiles[0] = append(workerFiles[0], files[i])
	}
	return workerFiles
}

// getModulePaths traverses the maven module tree and returns a flat list of module paths.
func (c *Collector) collectFields(node *sitter.Node, uri DocumentURI, content []byte, path string, parent *DocumentSymbol) {
	if node == nil {
		return
	}
	q, err := sitter.NewQuery([]byte("(field_declaration) @field"), parser.GetLanguage(c.CollectOption.Language))
	if err != nil {
		// Or handle the error more gracefully
		return
	}
	qc := sitter.NewQueryCursor()
	qc.Exec(q, node)

	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		for _, capture := range m.Captures {
			fieldNode := capture.Node
			// Find the type of the field.
			typeNode := fieldNode.ChildByFieldName("type")
			var typeDep dependency
			if typeNode != nil {
				typeSymbols := c.parseTypeIdentifiers(typeNode, content, uri)
				if len(typeSymbols) > 0 {
					// A variable has one type, we take the first symbol as its type.
					typeDep = dependency{Symbol: typeSymbols[0], Location: typeSymbols[0].Location}
				}
			}
			fullyName := fieldNode.Content(content)

			// A field declaration can have multiple variables, e.g., `int a, b;`
			// We need to iterate through the variable_declarator nodes.
			for i := 0; i < int(fieldNode.ChildCount()); i++ {
				child := fieldNode.Child(i)
				if child.Type() == "variable_declarator" {
					nameNode := child.ChildByFieldName("name")
					if nameNode == nil {
						continue
					}

					isStatic := strings.Contains(fullyName, "static")
					isFinal := strings.Contains(fullyName, "final")
					isPublic := strings.Contains(fullyName, "public")
					kind := SKUnknown
					if isStatic && isFinal && isPublic {
						kind = SKConstant
					} else if isStatic && isPublic {
						kind = SKVariable
					} else {
						kind = SKClass
					}

					if kind == SKClass {
						sym := typeDep.Symbol
						if sym == nil {
							continue
						}
						sym.Role = REFERENCE
						if parent != nil {
							c.addReferenceDeps(parent, sym)
						}
					} else {
						name := nameNode.Content(content)
						start := child.StartPoint()
						end := child.EndPoint()
						uri := NewURI(path)

						sym := &DocumentSymbol{
							Name: name,
							Kind: kind,
							Text: fullyName,
							Location: Location{
								URI: uri,
								Range: Range{
									Start: toLSPPosition(content, start.Row, start.Column),
									End:   toLSPPosition(content, end.Row, end.Column),
								},
							},
							Node:   child,
							Tokens: []Token{nodeToToken(child, content, uri)},
							Role:   REFERENCE,
						}
						if parent != nil {
							c.addReferenceDeps(parent, sym)
						}
						// Store the type dependency in c.vars
						if typeDep.Symbol != nil && kind == SKConstant || kind == SKVariable {
							c.vars[sym] = typeDep
							c.syms[sym.Location] = sym
						}
					}
				}
			}
		}
	}
}

func (c *Collector) addReferenceDeps(sym *DocumentSymbol, ref *DocumentSymbol) {
	if ref.Role != REFERENCE {
		return
	}
	TokenLocation := ref.Location
	var refDefinitionLocation = c.findDefinitionLocation(ref)
	if refDefinitionLocation == TokenLocation {
		// todo 三方外部符号查询不到，引用和定义符号位置一致时，过滤掉
		return
	}
	ref.Location = refDefinitionLocation
	c.deps[sym] = append(c.deps[sym], dependency{
		Symbol:   ref,
		Location: TokenLocation,
	})
}

func (c *Collector) findLocalLSPSymbol(fileURI DocumentURI) map[Range]*DocumentSymbol {
	if c.localLSPSymbol[fileURI] == nil {
		c.localLSPSymbol = make(map[DocumentURI]map[Range]*DocumentSymbol)
		symbols, _ := c.cli.DocumentSymbols(context.Background(), fileURI)
		c.localLSPSymbol[fileURI] = symbols
		return symbols
	}
	return c.localLSPSymbol[fileURI]
}

func (c *Collector) findDefinitionLocation(ref *DocumentSymbol) Location {
	defs, err := c.cli.Definition(context.Background(), ref.Location.URI, ref.Location.Range.Start)
	if err != nil || len(defs) == 0 {
		// 意味着引用为外部符号，LSP 无法查询到符号定位,暂时复用当前符号引用位置
		return ref.Location
	} else {
		return defs[0]
	}
}

func (c *Collector) walk(node *sitter.Node, uri DocumentURI, content []byte, file *uniast.File, parent *DocumentSymbol) {
	switch node.Type() {
	case "package_declaration":
		pkgNameNode := parser.FindChildIdentifier(node)
		if pkgNameNode != nil {
			file.Package = uniast.PkgPath(pkgNameNode.Content(content))
		}
		return // no need to walk children

	case "import_declaration":
		importPathNode := parser.FindChildIdentifier(node)
		if importPathNode != nil {
			file.Imports = append(file.Imports, uniast.Import{Path: importPathNode.Content(content)})
		}
		return // no need to walk children of import declaration

	case "class_declaration", "interface_declaration", "enum_declaration":
		nameNode := parser.FindChildIdentifier(node)
		if nameNode == nil {
			return // anonymous class, skip
		}
		name := nameNode.Content(content)
		start := node.StartPoint()
		end := node.EndPoint()

		var kind SymbolKind
		if node.Type() == "class_declaration" {
			kind = SKClass
		} else if node.Type() == "enum_declaration" {
			kind = SKEnum
		} else {
			kind = SKInterface
		}

		sym := &DocumentSymbol{
			Name: name,
			Kind: kind,
			Text: node.Content(content),
			Location: Location{
				URI: uri,
				Range: Range{
					Start: toLSPPosition(content, start.Row, start.Column),
					End:   toLSPPosition(content, end.Row, end.Column),
				},
			},
			Node: node,
			Role: DEFINITION,
		}

		symbols := c.findLocalLSPSymbol(sym.Location.URI)
		for _, symbol := range symbols {
			//lsp 替换
			if symbol.Name == name {
				sym.Location = symbol.Location
			}
		}

		// Collect tokens for class/interface declarations
		// Extract extends/implements for class_declaration
		if node.Type() == "class_declaration" {
			// Handle extends (superclass)
			extendsNode := node.ChildByFieldName("superclass")
			if extendsNode != nil {
				extendsType := c.parseTypeIdentifiers(extendsNode, content, uri)
				for _, ext := range extendsType {
					ext.Kind = SKClass
					ext.Role = REFERENCE
					c.addReferenceDeps(sym, ext)
				}
			}

			// Handle implements (interfaces)
			implementsNode := node.ChildByFieldName("interfaces")
			if implementsNode != nil {
				implTypes := c.parseTypeIdentifiers(implementsNode, content, uri)
				for _, impl := range implTypes {
					impl.Kind = SKInterface
					impl.Role = REFERENCE
					c.addReferenceDeps(sym, impl)
				}
			}
		}

		c.syms[sym.Location] = sym
		if parent != nil {
			parent.Children = append(parent.Children, sym)
			c.deps[parent] = append(c.deps[parent], dependency{
				Symbol:   sym,
				Location: sym.Location,
			})

		}

		// walk children
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil {
			c.collectFields(bodyNode, uri, content, uri.File(), sym)
			for i := 0; i < int(bodyNode.ChildCount()); i++ {
				child := bodyNode.Child(i)
				c.walk(child, uri, content, file, sym)
			}
		}
		return // children already walked

	case "method_declaration":
		nameNode := node.ChildByFieldName("name")
		if nameNode == nil {
			return // Can be a constructor
		}
		name := nameNode.Content(content)
		start := node.StartPoint()
		end := node.EndPoint()

		isStatic := isStaticMethod(node, content)

		// 根据是否为静态方法设置不同的Kind
		var kind SymbolKind
		if isStatic {
			kind = SKFunction // 静态方法 -> Functions
		} else {
			kind = SKMethod // 非静态方法 -> type的method
		}

		sym := &DocumentSymbol{
			Name: name,
			Kind: kind,
			Text: node.Content(content),
			Location: Location{
				URI: uri,
				Range: Range{
					Start: toLSPPosition(content, start.Row, start.Column),
					End:   toLSPPosition(content, end.Row, end.Column),
				},
			},
			Node: node,
			Role: DEFINITION,
		}

		symbols := c.findLocalLSPSymbol(sym.Location.URI)
		signature := c.parseMethodSignature(node, content)
		for _, symbol := range symbols {
			if symbol.Name == signature {
				sym.Location = symbol.Location
				sym.Name = symbol.Name
			}
		}

		info := functionInfo{
			TypeParams: make(map[int]dependency),
			Inputs:     make(map[int]dependency),
			Outputs:    make(map[int]dependency),
		}

		// Parse type parameters
		if typeParamsNode := node.ChildByFieldName("type_parameters"); typeParamsNode != nil {
			typeParams := c.parseTypeIdentifiers(typeParamsNode, content, uri)
			for i, p := range typeParams {
				p.Kind = SKTypeParameter
				p.Role = REFERENCE
				tokenLocation := p.Location
				p.Location = c.findDefinitionLocation(p)
				if tokenLocation == p.Location {
					// 外部依赖符号，跳过
					continue
				}
				info.TypeParams[i] = dependency{Symbol: p,
					Location: tokenLocation,
				}
			}
		}

		// Parse return type and add to tokens
		if returnTypeNode := node.ChildByFieldName("type"); returnTypeNode != nil {
			returns := c.parseTypeIdentifiers(returnTypeNode, content, uri)
			for i, p := range returns {
				p.Role = REFERENCE
				tokenLocation := p.Location
				p.Location = c.findDefinitionLocation(p)
				if tokenLocation == p.Location {
					// 外部依赖符号，跳过
					continue
				}
				info.Outputs[i] = dependency{Symbol: p, Location: tokenLocation}
			}
		}

		// Parse parameters and add to tokens
		if paramsNode := node.ChildByFieldName("parameters"); paramsNode != nil {
			params := c.parseFormalParameters(paramsNode, content, uri)
			for i, p := range params {
				if typeNode := p.Node.ChildByFieldName("type"); typeNode != nil {
					typeSymbols := c.parseTypeIdentifiers(typeNode, content, uri)
					for _, typeSym := range typeSymbols {
						typeSym.Role = REFERENCE
						tokenLocation := typeSym.Location
						typeSym.Location = c.findDefinitionLocation(typeSym)
						if tokenLocation == p.Location {
							// 外部依赖符号，跳过
							continue
						}
						info.Inputs[i] = dependency{Symbol: typeSym, Location: tokenLocation}
					}
				}
			}
		}

		// Populate Method info
		if parent != nil && (parent.Kind == SKClass || parent.Kind == SKInterface) {
			info.Method = &methodInfo{
				Receiver: dependency{Symbol: parent, Location: parent.Location},
			}
		}

		// Sort dependencies
		if len(info.TypeParams) > 0 {
			keys := make([]int, 0, len(info.TypeParams))
			for k := range info.TypeParams {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			info.TypeParamsSorted = make([]dependency, len(keys))
			for i, k := range keys {
				info.TypeParamsSorted[i] = info.TypeParams[k]
			}
		}
		if len(info.Outputs) > 0 {
			keys := make([]int, 0, len(info.Outputs))
			for k := range info.Outputs {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			info.OutputsSorted = make([]dependency, len(keys))
			for i, k := range keys {
				info.OutputsSorted[i] = info.Outputs[k]
			}
		}
		if len(info.Inputs) > 0 {
			keys := make([]int, 0, len(info.Inputs))
			for k := range info.Inputs {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			info.InputsSorted = make([]dependency, len(keys))
			for i, k := range keys {
				info.InputsSorted[i] = info.Inputs[k]
			}
		}

		// Generate signature
		var signatureEnd uint32
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil {
			signatureEnd = bodyNode.StartByte()
			// 解析方法体内的所有方法调用
			c.parseMethodInvocations(bodyNode, content, uri, sym)
		} else {
			signatureEnd = node.EndByte()
		}
		info.Signature = strings.TrimSpace(string(content[node.StartByte():signatureEnd]))
		c.funcs[sym] = info
		c.syms[sym.Location] = sym

		return // children already walked

	case "field_declaration":
		return
	}

	// default behavior
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		c.walk(child, uri, content, file, parent)
	}
}

// parseTypeIdentifiers walks through a node (like type_parameters or a return type node)
// and extracts all type identifiers, creating placeholder DocumentSymbols for them.
func (c *Collector) parseTypeIdentifiers(node *sitter.Node, content []byte, uri DocumentURI) []*DocumentSymbol {
	var symbols []*DocumentSymbol
	c.recursiveParseTypes(node, content, uri, &symbols, false)
	return symbols
}

func (c *Collector) recursiveParseTypes(node *sitter.Node, content []byte, uri DocumentURI, symbols *[]*DocumentSymbol, IsInterface bool) {
	switch node.Type() {
	case "generic_type":

		// This is a base case for the recursion.
		start := node.StartPoint()
		end := node.EndPoint()
		kind := java.NodeTypeToSymbolKind(node.Type())

		typeSym := &DocumentSymbol{
			Name: node.Content(content),
			Kind: kind,
			Location: Location{
				URI: uri,
				Range: Range{
					Start: toLSPPosition(content, start.Row, start.Column),
					End:   toLSPPosition(content, end.Row, end.Column),
				},
			},
			Text: node.Content(content),
			Node: node,
		}
		*symbols = append(*symbols, typeSym)

		// For a generic type like "List<String>", we want to parse "List" and "String" separately.
		// The main type identifier (e.g., "List")
		typeNode := parser.FindChildByType(node, "type")
		if typeNode != nil {
			c.recursiveParseTypes(typeNode, content, uri, symbols, false)
		}
		// The type arguments (e.g., "<String>")
		argsNode := parser.FindChildByType(node, "type_arguments")
		if argsNode != nil {
			for i := 0; i < int(argsNode.ChildCount()); i++ {
				c.recursiveParseTypes(argsNode.Child(i), content, uri, symbols, false)
			}
		}
	case "type_identifier":
		// This is a base case for the recursion.
		start := node.StartPoint()
		end := node.EndPoint()
		kind := java.NodeTypeToSymbolKind(node.Type())
		if IsInterface {
			kind = SKInterface
		}
		typeSym := &DocumentSymbol{
			Name: node.Content(content),
			Kind: kind,
			Location: Location{
				URI: uri,
				Range: Range{
					Start: toLSPPosition(content, start.Row, start.Column),
					End:   toLSPPosition(content, end.Row, end.Column),
				},
			},
			Text: node.Content(content),
			Node: node,
		}
		*symbols = append(*symbols, typeSym)
	case "super_interfaces":
		typeNode := parser.FindChildByType(node, "type_list")
		if typeNode != nil {
			c.recursiveParseTypes(typeNode, content, uri, symbols, true)
		}
	default:
		// For any other node type, recurse on its children.
		for i := 0; i < int(node.ChildCount()); i++ {
			c.recursiveParseTypes(node.Child(i), content, uri, symbols, IsInterface)
		}
	}
}

// parseFormalParameters handles the `formal_parameters` node to extract each parameter.
func (c *Collector) parseFormalParameters(node *sitter.Node, content []byte, uri DocumentURI) []*DocumentSymbol {
	var symbols []*DocumentSymbol

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "formal_parameter" {

			paramTypeNode := child.ChildByFieldName("type")
			paramNameNode := child.ChildByFieldName("name")
			if paramTypeNode != nil && paramNameNode != nil {
				start := child.StartPoint()
				end := child.EndPoint()
				paramSym := &DocumentSymbol{
					Name: paramNameNode.Content(content),
					Kind: java.NodeTypeToSymbolKind(paramTypeNode.Type()),
					Location: Location{
						URI: uri,
						Range: Range{
							Start: toLSPPosition(content, start.Row, start.Column),
							End:   toLSPPosition(content, end.Row, end.Column),
						},
					},
					Text: child.Content(content),
					Node: child,
				}
				symbols = append(symbols, paramSym)
			}
		}
	}
	return symbols
}

func isStaticMethod(node *sitter.Node, content []byte) bool {
	var modifiersNode *sitter.Node
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "modifiers" {
			modifiersNode = child
			break
		}
	}

	if modifiersNode == nil {
		return false
	}
	modifiersString := modifiersNode.Content(content)
	return strings.Contains(modifiersString, "static")
}

func (c *Collector) internal(loc Location) bool {
	return strings.HasPrefix(loc.URI.File(), c.repo)
}

func (c *Collector) getSymbolByToken(ctx context.Context, tok Token) (*DocumentSymbol, error) {
	return c.getSymbolByTokenWithLimit(ctx, tok, 1)
}

func (c *Collector) getSymbolByTokenWithLimit(ctx context.Context, tok Token, depth int) (*DocumentSymbol, error) {
	// get definition symbol
	defs, err := c.cli.Definition(ctx, tok.Location.URI, tok.Location.Range.Start)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("definition of token %s not found", tok)
	}
	if len(defs) > 1 {
		log.Error("definition of token %s not unique", tok)
	}
	return c.getSymbolByLocation(ctx, defs[0], depth, tok)
}

// Find the symbol (from the symbol list) that matches the location.
// It is the smallest (most specific) entity symbol that contains the location.
//
// Parameters:
//
//	@syms: the list of symbols to search in
//	@loc: the location to find the symbol for
//
// Returns:
//
//	*DocumentSymbol: the most specific entity symbol that contains the location.
//	If no such symbol is found, it returns nil.
func (c *Collector) findMatchingSymbolIn(loc Location, syms []*DocumentSymbol) *DocumentSymbol {
	var most_specific *DocumentSymbol
	for _, sym := range syms {
		if !sym.Location.Include(loc) || !c.spec.IsEntitySymbol(*sym) {
			continue
		}
		// now we have a candidate (containing loc && entity), check if it is the most specific
		if most_specific == nil {
			most_specific = sym
			continue
		}
		if most_specific.Location.Include(sym.Location) {
			// use sym, which is more specific than most_specific
			most_specific = sym
			continue
		}
		if sym.Location.Include(most_specific.Location) {
			// remain current choice
			continue
		}
		// Indicates a bad usage, sym contains unstructured symbols.
		log.Error("getMostSpecificEntitySymbol: cannot decide between symbols %s (at %+v) and %s (at %+v)\n",
			most_specific.Name, most_specific.Location,
			sym.Name, sym.Location)
	}
	return most_specific
}

// return a language entity symbol
//   - loaded: just return loaded symbol
//   - not loaded but set option LoadExternalSymbol: load external symbol and return
//   - otherwise: return a Unknown symbol
func (c *Collector) getSymbolByLocation(ctx context.Context, loc Location, depth int, from Token) (*DocumentSymbol, error) {
	// already loaded
	// if sym, ok := c.syms[loc]; ok {
	// 	return sym, nil
	// }

	// 1. already loaded
	if sym := c.findMatchingSymbolIn(loc, slices.Collect(maps.Values(c.syms))); sym != nil {
		return sym, nil
	}

	if c.LoadExternalSymbol && !c.internal(loc) && (c.NeedStdSymbol || !c.spec.IsStdToken(from)) {
		// 2. load external symbol from its file
		syms, err := c.cli.DocumentSymbols(ctx, loc.URI)
		if err != nil {
			return nil, err
		}
		// load the other external symbols in that file
		for _, sym := range syms {
			// save symbol first
			if _, ok := c.syms[sym.Location]; !ok {
				content, err := c.cli.Locate(sym.Location)
				if err != nil {
					return nil, err
				}
				sym.Text = content
				c.syms[sym.Location] = sym
			}
		}
		// load more external symbols if depth permits
		if depth >= 0 {
			// process target symbol
			for _, sym := range syms {
				// check if need process
				if c.needProcessExternal(sym) {
					// collect tokens before process
					tokens, err := c.cli.SemanticTokens(ctx, sym.Location)
					if err != nil {
						return nil, err
					}
					sym.Tokens = tokens
					c.processSymbol(ctx, sym, depth-1)
				}
			}
		}
		rsym := c.findMatchingSymbolIn(loc, slices.Collect(maps.Values(syms)))
		return rsym, nil
	} else {
		// external symbol, just locate the content
		var text string
		if c.internal(loc) {
			// maybe internal symbol not loaded, like `lazy_static!` in Rust
			// use the before and after symbol as text
			var left, right *DocumentSymbol
			syms, err := c.cli.DocumentSymbols(ctx, loc.URI)
			if err != nil {
				if c.cli.ClientOptions.Verbose {
					log.Error("locate %v failed: %v\n", loc, err)
				}
				goto finally
			}
			for _, sym := range syms {
				if sym.Location.Range.End.Less(loc.Range.Start) {
					if left == nil || left.Location.Range.End.Less(sym.Location.Range.End) {
						left = sym
					}
				}
				if loc.Range.End.Less(sym.Location.Range.Start) {
					if right == nil || sym.Location.Range.Start.Less(right.Location.Range.Start) {
						right = sym
					}
				}
			}
			if left == nil {
				left = &DocumentSymbol{
					Location: Location{
						URI: loc.URI,
						Range: Range{
							Start: Position{
								Line:      0,
								Character: 0,
							},
							End: Position{
								Line:      0,
								Character: 0,
							},
						},
					},
				}
			}
			if right == nil {
				lines := c.cli.LineCounts(loc.URI)
				right = &DocumentSymbol{
					Location: Location{
						URI: loc.URI,
						Range: Range{
							Start: Position{
								Line:      len(lines),
								Character: 1,
							},
							End: Position{
								Line:      len(lines),
								Character: 1,
							},
						},
					},
				}
			}
			var end int
			line := c.cli.Line(loc.URI, right.Location.Range.Start.Line-1)
			for i := 0; i < len(line); i++ {
				if unicode.IsSpace(rune(line[i])) {
					end = i
					break
				}
			}
			txt, err := c.cli.Locate(Location{
				URI: loc.URI,
				Range: Range{
					Start: Position{
						Line:      left.Location.Range.End.Line + 1,
						Character: 0,
					},
					End: Position{
						Line:      right.Location.Range.Start.Line - 1,
						Character: end,
					},
				},
			})
			if err != nil {
				if c.cli.ClientOptions.Verbose {
					log.Error("locate %v failed: %v\n", loc, err)
				}
				goto finally
			}
			text = txt
		}
	finally:
		if text == "" {
			txt, err := c.cli.Locate(loc)
			if err != nil {
				if c.cli.ClientOptions.Verbose {
					log.Error("locate %v failed: %v\n", loc, err)
				}
			}
			text = txt
		}
		// not loaded, make a fake Unknown symbol
		tmp := &DocumentSymbol{
			Name:     from.Text,
			Kind:     c.spec.TokenKind(from),
			Location: loc,
			Text:     text,
		}
		c.syms[loc] = tmp
		return tmp, nil
	}
}

func (c *Collector) getDepsWithLimit(ctx context.Context, sym *DocumentSymbol, tps []int, depth int) (map[int]dependency, []dependency) {
	var tsyms = make(map[int]dependency, len(tps))
	var sorted = make([]dependency, 0, len(tps))
	for _, tp := range tps {
		dep, err := c.getSymbolByTokenWithLimit(ctx, sym.Tokens[tp], depth)
		if err != nil || sym == nil {
			log.Error_skip(1, "token %v not found its symbol: %v", tp, err)
		} else {
			d := dependency{sym.Tokens[tp].Location, dep}
			tsyms[tp] = d
			sorted = append(sorted, d)
		}
	}
	return tsyms, sorted
}

func (c *Collector) collectImpl(ctx context.Context, sym *DocumentSymbol, depth int) {
	// method info: receiver, implementee
	inter, rec, fn := c.spec.ImplSymbol(*sym)
	if rec < 0 {
		return
	}
	var rd, ind *dependency
	var err error
	rsym, err := c.getSymbolByTokenWithLimit(ctx, sym.Tokens[rec], depth)
	if err != nil || rsym == nil {
		log.Error("get receiver symbol for token %v failed: %v\n", rec, err)
		return
	}
	rd = &dependency{sym.Tokens[rec].Location, rsym}
	if inter >= 0 {
		isym, err := c.getSymbolByToken(ctx, sym.Tokens[inter])
		if err != nil || isym == nil {
			log.Error("get implement symbol for token %v failed: %v\n", inter, err)
		} else {
			ind = &dependency{sym.Tokens[inter].Location, isym}
		}
	}
	var impl string
	// HACK: impl head for Rust.
	if fn > 0 && fn < len(sym.Tokens) {
		impl = ChunkHead(sym.Text, sym.Location.Range.Start, sym.Tokens[fn].Location.Range.Start)
	}
	// HACK: implhead for Python. Should actually be provided by the language spec.
	if impl == "" || len(impl) < len(sym.Name) {
		impl = fmt.Sprintf("class %s {\n", sym.Name)
	}
	// search all methods
	for _, method := range c.syms {
		// NOTICE: some class method (ex: XXType::new) are SKFunction, but still collect its receiver
		if (method.Kind == SKMethod || method.Kind == SKFunction) && sym.Location.Include(method.Location) {
			if _, ok := c.funcs[method]; !ok {
				c.funcs[method] = functionInfo{}
			}
			f := c.funcs[method]
			f.Method = &methodInfo{
				Receiver:  *rd,
				Interface: ind,
				ImplHead:  impl,
			}
			c.funcs[method] = f
		}
	}
}

func (c *Collector) needProcessExternal(sym *DocumentSymbol) bool {
	return (c.spec.HasImplSymbol() && sym.Kind == SKObject) || (!c.spec.HasImplSymbol() && sym.Kind == SKMethod)
}

func (c *Collector) processSymbol(ctx context.Context, sym *DocumentSymbol, depth int) {
	// method info: receiver, implementee
	hasImpl := c.spec.HasImplSymbol()
	if hasImpl {
		c.collectImpl(ctx, sym, depth)
	}

	// function info: type params, inputs, outputs, receiver (if !needImpl)
	if sym.Kind == SKFunction || sym.Kind == SKMethod {
		var rsym *dependency
		rec, tps, ips, ops := c.spec.FunctionSymbol(*sym)

		if !hasImpl && rec >= 0 {
			rsym, err := c.getSymbolByTokenWithLimit(ctx, sym.Tokens[rec], depth)
			if err != nil || rsym == nil {
				log.Error("get receiver symbol for token %v failed: %v\n", rec, err)
			}
		}
		tsyms, ts := c.getDepsWithLimit(ctx, sym, tps, depth-1)
		ipsyms, is := c.getDepsWithLimit(ctx, sym, ips, depth-1)
		opsyms, os := c.getDepsWithLimit(ctx, sym, ops, depth-1)

		//get last token of params for get signature
		lastToken := rec
		for _, t := range tps {
			if t > lastToken {
				lastToken = t
			}
		}
		for _, t := range ips {
			if t > lastToken {
				lastToken = t
			}
		}
		for _, t := range ops {
			if t > lastToken {
				lastToken = t
			}
		}

		c.updateFunctionInfo(sym, tsyms, ipsyms, opsyms, ts, is, os, rsym, lastToken)
	}

	// variable info: type
	if sym.Kind == SKVariable || sym.Kind == SKConstant {
		i := c.spec.DeclareTokenOfSymbol(*sym)
		// find first entity token
		for i = i + 1; i < len(sym.Tokens); i++ {
			if c.spec.IsEntityToken(sym.Tokens[i]) {
				break
			}
		}
		if i < 0 || i >= len(sym.Tokens) {
			log.Error("get type token of variable symbol %s failed\n", sym)
			return
		}
		tsym, err := c.getSymbolByTokenWithLimit(ctx, sym.Tokens[i], depth-1)
		if err != nil || tsym == nil {
			log.Error("get type symbol for token %s failed:%v\n", sym.Tokens[i], err)
			return
		}
		c.vars[sym] = dependency{
			Location: sym.Tokens[i].Location,
			Symbol:   tsym,
		}
	}
}

func (c *Collector) updateFunctionInfo(sym *DocumentSymbol, tsyms, ipsyms, opsyms map[int]dependency, ts, is, os []dependency, rsym *dependency, lastToken int) {
	if _, ok := c.funcs[sym]; !ok {
		c.funcs[sym] = functionInfo{}
	}
	f := c.funcs[sym]
	f.TypeParams = tsyms
	f.TypeParamsSorted = ts
	f.Inputs = ipsyms
	f.InputsSorted = is
	f.Outputs = opsyms
	f.OutputsSorted = os
	if rsym != nil {
		if f.Method == nil {
			f.Method = &methodInfo{}
		}
		f.Method.Receiver = *rsym
	}

	// ctruncate the function signature text
	if lastToken >= 0 && lastToken < len(sym.Tokens)-1 {
		lastPos := sym.Tokens[lastToken+1].Location.Range.Start
		f.Signature = ChunkHead(sym.Text, sym.Location.Range.Start, lastPos)
	}

	c.funcs[sym] = f
}

// nodeToLocation converts a Tree-sitter node's position information to LSP Location format.
func nodeToLocation(node *sitter.Node, uri DocumentURI, content []byte) Location {
	start := node.StartPoint()
	end := node.EndPoint()

	// 将Tree-sitter的UTF-8字节位置转换为LSP的UTF-16字符位置
	startLine, startChar := parser.Utf8ToUtf16Position(content, start.Row, start.Column)
	endLine, endChar := parser.Utf8ToUtf16Position(content, end.Row, end.Column)

	return Location{
		URI: uri,
		Range: Range{
			Start: Position{Line: startLine, Character: startChar},
			End:   Position{Line: endLine, Character: endChar},
		},
	}
}

func toLSPPosition(content []byte, Row, Column uint32) Position {
	startLine, startChar := parser.Utf8ToUtf16Position(content, Row, Column)
	return Position{Line: startLine, Character: startChar}
}

// nodeToToken converts a Tree-sitter node to lsp.Token.
func nodeToToken(node *sitter.Node, content []byte, uri DocumentURI) Token {
	// Validate node position to ensure it's within file bounds
	start := node.StartPoint()
	end := node.EndPoint()

	// Ensure position is valid for LSP
	if start.Row < 0 || start.Column < 0 || end.Row < 0 || end.Column < 0 {
		// Log warning for invalid position
		log.Error("Invalid Tree-sitter position: node=%s, start=%d:%d, end=%d:%d",
			node.Type(), start.Row, start.Column, end.Row, end.Column)
	}

	return Token{
		Text:      node.Content(content),
		Location:  nodeToLocation(node, uri, content),
		Type:      node.Type(),
		Modifiers: []string{}, // Initialize with empty slice to avoid nil
	}
}

func (c *Collector) parseMethodInvocations(bodyNode *sitter.Node, content []byte, uri DocumentURI, methodSym *DocumentSymbol) {
	if bodyNode == nil {
		return
	}

	// New approach: find argument_list, then find its parent (method_invocation)
	// and extract name and object from there.
	query, err := sitter.NewQuery([]byte(`
		(argument_list) @args
	`), parser.GetLanguage(c.CollectOption.Language))
	if err != nil {
		log.Error("Failed to create method invocation query: %v", err)
		return
	}
	defer query.Close()

	qc := sitter.NewQueryCursor()
	defer qc.Close()
	qc.Exec(query, bodyNode)

	for {
		match, ok := qc.NextMatch()
		if !ok {
			break
		}

		for _, capture := range match.Captures {
			argListNode := capture.Node

			invocationNode := argListNode.Parent()
			if invocationNode == nil || invocationNode.Type() != "method_invocation" {
				continue
			}

			methodNameNode := invocationNode.ChildByFieldName("name")
			if methodNameNode == nil {
				continue
			}

			methodName := methodNameNode.Content(content)
			start := methodNameNode.StartPoint()
			end := methodNameNode.EndPoint()
			invocationLocation := Location{
				URI: uri,
				Range: Range{
					Start: toLSPPosition(content, start.Row, start.Column),
					End:   toLSPPosition(content, end.Row, end.Column),
				},
			}

			objectNode := invocationNode.ChildByFieldName("object")

			var dep dependency

			if objectNode != nil {
				// This could be a static or a normal method call.
				className := c.extractRootIdentifier(objectNode, content)
				// A simple heuristic to decide if it's a static call:
				// if the extracted root identifier is not empty and starts with an uppercase letter.
				// This is not foolproof but a common convention in Java.
				isStatic := false
				if className != "" {
					runes := []rune(className)
					if len(runes) > 0 && unicode.IsUpper(runes[0]) {
						isStatic = true
					}
				}

				if isStatic {
					// Static method call
					qualifiedMethodName := className + "." + methodName
					dep = dependency{
						Symbol: &DocumentSymbol{
							Name:     qualifiedMethodName,
							Kind:     SKFunction,
							Location: invocationLocation,
							Role:     REFERENCE,
						},
						Location: invocationLocation,
					}
				} else {
					dep = dependency{
						Symbol: &DocumentSymbol{
							Name:     methodName,
							Kind:     SKMethod,
							Location: invocationLocation,
							Role:     REFERENCE,
						},
						Location: invocationLocation,
					}
				}
			} else {
				dep = dependency{
					Symbol: &DocumentSymbol{
						Name:     methodName,
						Kind:     SKMethod,
						Location: invocationLocation,
						Role:     REFERENCE,
					},
					Location: invocationLocation,
				}
			}
			DefinitionLocation := c.findDefinitionLocation(dep.Symbol)

			if DefinitionLocation == dep.Symbol.Location {
				//外部函数调用，先过滤
				continue
			}
			dep.Symbol.Location = DefinitionLocation
			c.deps[methodSym] = append(c.deps[methodSym], dep)
		}
	}
}

func (c *Collector) extractRootIdentifier(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}

	if node.Type() == "identifier" {
		return node.Content(content)
	}

	childCount := int(node.ChildCount())
	for i := 0; i < childCount; i++ {
		child := node.Child(i)
		fieldName := node.FieldNameForChild(i)
		if fieldName == "object" {
			return c.extractRootIdentifier(child, content)
		}
	}

	// Fallback for cases where the field name is not 'object'
	if childCount > 0 {
		return c.extractRootIdentifier(node.Child(0), content)
	}

	return ""
}

// parseMethodSignature 从方法节点解析签名，保留方法名和参数类型
// 例如: public String queryJwtToken(String id, String tenantId, String idType) -> queryJwtToken(String, String, String)
// 例如: forwardLarkEvent(Map<String, Object>) -> forwardLarkEvent(Map<String, Object>)
func (c *Collector) parseMethodSignature(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}

	// 获取方法名
	nameNode := parser.FindChildIdentifier(node)
	if nameNode == nil {
		return ""
	}
	methodName := nameNode.Content(content)

	// 获取参数节点
	paramsNode := node.ChildByFieldName("parameters")
	if paramsNode == nil {
		return fmt.Sprintf("%s()", methodName)
	}
	// 解析参数类型
	var paramTypes []string

	// 遍历所有参数
	for i := 0; i < int(paramsNode.ChildCount()); i++ {
		child := paramsNode.Child(i)
		if child.Type() == "formal_parameter" {
			// 获取参数类型节点
			typeNode := child.ChildByFieldName("type")
			if typeNode != nil {
				typeContent := typeNode.Content(content)
				if typeContent != "" {
					paramTypes = append(paramTypes, typeContent)
				}
			}
		} else if child.Type() == "spread_parameter" {
			for u := range int(child.ChildCount()) {
				// 处理可变参数 ...Type
				parameterNode := child.Child(u)
				if parameterNode != nil && parameterNode.Type() == "type_identifier" {
					paramType := parameterNode.Content(content)
					if paramType != "" {
					}
					paramTypes = append(paramTypes, paramType+"...")
				}
			}

		}
	}

	return fmt.Sprintf("%s(%s)", methodName, strings.Join(paramTypes, ", "))
}
