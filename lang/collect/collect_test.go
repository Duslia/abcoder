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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/abcoder/lang/java"
	javaLsp "github.com/cloudwego/abcoder/lang/java/lsp"
	"github.com/cloudwego/abcoder/lang/log"
	"github.com/cloudwego/abcoder/lang/lsp"
	"github.com/cloudwego/abcoder/lang/testutils"
	"github.com/cloudwego/abcoder/lang/uniast"
)

func TestCollector_CollectByTreeSitter_Java(t *testing.T) {
	log.SetLogLevel(log.DebugLevel)
	javaTestCase := "../../testdata/java/1_advanced"

	t.Run("javaCollect", func(t *testing.T) {

		lsp.RegisterProvider(uniast.Java, &javaLsp.JavaProvider{})

		openfile, wait := java.CheckRepo(javaTestCase)
		l, s := java.GetDefaultLSP(make(map[string]interface{}))
		client, err := lsp.NewLSPClient(javaTestCase, openfile, wait, lsp.ClientOptions{
			Server:   s,
			Language: l,
			Verbose:  false,
		})

		c := NewCollector(javaTestCase, client)
		c.Language = uniast.Java
		_, err = c.ScannerByTreeSitter(context.Background())
		if err != nil {
			t.Fatalf("Collector.CollectByTreeSitter() failed = %v\n", err)
		}

		if len(c.files) == 0 {
			t.Fatalf("Expected have file, but got %d", len(c.files))
		}

		expectedFile := filepath.Join(javaTestCase, "/src/main/java/org/example/test.json")
		if _, ok := c.files[expectedFile]; ok {
			t.Fatalf("Expected file %s not found", expectedFile)
		}
	})
}

// TestCollector_ConcurrencyConfig 测试并发配置功能
func TestCollector_ConcurrencyConfig(t *testing.T) {
	log.SetLogLevel(log.DebugLevel)
	javaTestCase := "../../testdata/java/1_advanced"

	t.Run("concurrencyConfig", func(t *testing.T) {
		lsp.RegisterProvider(uniast.Java, &javaLsp.JavaProvider{})

		// 创建单个LSP客户端
		openfile, wait := java.CheckRepo(javaTestCase)
		l, s := java.GetDefaultLSP(make(map[string]interface{}))
		client, err := lsp.NewLSPClient(javaTestCase, openfile, wait, lsp.ClientOptions{
			Server:   s,
			Language: l,
			Verbose:  false,
		})
		if err != nil {
			t.Fatalf("Failed to create LSP client: %v", err)
		}
		defer client.Close()

		// 测试基本收集器
		collector := NewCollector(javaTestCase, client)
		collector.Language = uniast.Java

		// 测试默认配置
		maxWorkers, enableSorting, bufferSize := collector.GetConcurrencyConfig()
		t.Logf("Default config: workers=%d, sorting=%v, buffer=%d", maxWorkers, enableSorting, bufferSize)

		// 测试设置配置
		collector.SetConcurrencyConfig(2, false, 5)
		maxWorkers, enableSorting, bufferSize = collector.GetConcurrencyConfig()
		if maxWorkers != 2 || enableSorting != false || bufferSize != 5 {
			t.Errorf("Config setting failed: expected (2, false, 5), got (%d, %v, %d)",
				maxWorkers, enableSorting, bufferSize)
		}

		// 测试基本符号收集
		start := time.Now()
		symbols, err := collector.ScannerByTreeSitter(context.Background())
		duration := time.Since(start)
		if err != nil {
			t.Fatalf("Symbol collection failed: %v", err)
		}

		t.Logf("Processing time: %v", duration)
		t.Logf("Symbols count: %d", len(symbols))

		if len(symbols) == 0 {
			t.Error("Expected some symbols to be collected")
		}
	})
}

// TestCollector_ClientPool 测试客户端池功能
func TestCollector_ClientPool(t *testing.T) {
	log.SetLogLevel(log.DebugLevel)
	javaTestCase := "../../testdata/java/1_advanced"

	t.Run("clientPoolManagement", func(t *testing.T) {
		lsp.RegisterProvider(uniast.Java, &javaLsp.JavaProvider{})

		// 创建多个LSP客户端
		var clients []*lsp.LSPClient
		for i := 0; i < 2; i++ {
			openfile, wait := java.CheckRepo(javaTestCase)
			l, s := java.GetDefaultLSP(make(map[string]interface{}))
			client, err := lsp.NewLSPClient(javaTestCase, openfile, wait, lsp.ClientOptions{
				Server:   s,
				Language: l,
				Verbose:  false,
			})
			if err != nil {
				t.Fatalf("Failed to create LSP client %d: %v", i, err)
			}
			clients = append(clients, client)
		}
		defer func() {
			for _, client := range clients {
				client.Close()
			}
		}()

		// 创建客户端池
		pool := NewLSPClientPool(clients)
		if pool.Size() != 2 {
			t.Errorf("Expected pool size 2, got %d", pool.Size())
		}
		if pool.Available() != 2 {
			t.Errorf("Expected 2 available clients, got %d", pool.Available())
		}

		// 测试客户端获取和释放
		client1 := pool.Acquire()
		if client1 == nil {
			t.Fatal("Failed to acquire client from pool")
		}
		if pool.Available() != 1 {
			t.Errorf("Expected 1 available client after acquire, got %d", pool.Available())
		}

		client2 := pool.Acquire()
		if client2 == nil {
			t.Fatal("Failed to acquire second client from pool")
		}
		if pool.Available() != 0 {
			t.Errorf("Expected 0 available clients after acquiring all, got %d", pool.Available())
		}

		// 释放客户端
		pool.Release(client1)
		if pool.Available() != 1 {
			t.Errorf("Expected 1 available client after release, got %d", pool.Available())
		}

		pool.Release(client2)
		if pool.Available() != 2 {
			t.Errorf("Expected 2 available clients after releasing all, got %d", pool.Available())
		}

		// 测试使用池的收集器
		collector := NewCollectorWithPool(javaTestCase, pool)
		collector.Language = uniast.Java

		symbols, err := collector.ScannerByTreeSitter(context.Background())
		if err != nil {
			t.Fatalf("Collector with pool failed: %v", err)
		}

		if len(symbols) == 0 {
			t.Error("Expected symbols from collector with pool")
		}
	})
}

// BenchmarkCollector_Sequential 基准测试：顺序处理
func BenchmarkCollector_Sequential(b *testing.B) {
	log.SetLogLevel(log.ErrorLevel) // 减少日志输出
	javaTestCase := "../../testdata/java/1_advanced"

	lsp.RegisterProvider(uniast.Java, &javaLsp.JavaProvider{})

	// 创建LSP客户端
	openfile, wait := java.CheckRepo(javaTestCase)
	l, s := java.GetDefaultLSP(make(map[string]interface{}))
	client, err := lsp.NewLSPClient(javaTestCase, openfile, wait, lsp.ClientOptions{
		Server:   s,
		Language: l,
		Verbose:  false,
	})
	if err != nil {
		b.Fatalf("Failed to create LSP client: %v", err)
	}
	defer client.Close()

	collector := NewCollector(javaTestCase, client)
	collector.Language = uniast.Java

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := collector.ScannerByTreeSitter(context.Background())
		if err != nil {
			b.Fatalf("Sequential processing failed: %v", err)
		}
	}
}

// BenchmarkCollector_Concurrent 基准测试：并发处理
func TestCollector_ConcurrentWithoutBuffer(t *testing.T) {
	repoPath := "../../testdata/java/4_full_maven_repo"

	// 创建多个LSP客户端
	var clients []*lsp.LSPClient
	for i := 0; i < 2; i++ {
		openfile, wait := java.CheckRepo(repoPath)
		l, s := java.GetDefaultLSP(make(map[string]interface{}))
		client, err := lsp.NewLSPClient(repoPath, openfile, wait, lsp.ClientOptions{
			Server:   s,
			Language: l,
			Verbose:  false,
		})
		if err != nil {
			t.Fatalf("Failed to create LSP client %d: %v", i, err)
		}
		clients = append(clients, client)
	}
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()

	// 创建并发收集器
	collector := NewCollectorWithMultipleClients(repoPath, clients)
	collector.Language = uniast.Java

	// 设置无缓冲区配置
	collector.SetConcurrencyConfig(2, true, 0) // bufferSize = 0

	// 执行符号收集
	start := time.Now()
	symbols, err := collector.ScannerByTreeSitter(context.Background())
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Symbol collection failed: %v", err)
	}

	t.Logf("Processing time with no buffer: %v", duration)
	t.Logf("Symbols count: %d", len(symbols))

	if len(symbols) == 0 {
		t.Error("Expected to collect some symbols")
	}
}

func BenchmarkCollector_Concurrent(b *testing.B) {
	log.SetLogLevel(log.ErrorLevel) // 减少日志输出
	javaTestCase := "../../testdata/java/4_full_maven_repo"

	lsp.RegisterProvider(uniast.Java, &javaLsp.JavaProvider{})

	// 创建多个LSP客户端
	var clients []*lsp.LSPClient
	for i := 0; i < 3; i++ {
		openfile, wait := java.CheckRepo(javaTestCase)
		l, s := java.GetDefaultLSP(make(map[string]interface{}))
		client, err := lsp.NewLSPClient(javaTestCase, openfile, wait, lsp.ClientOptions{
			Server:   s,
			Language: l,
			Verbose:  false,
		})
		if err != nil {
			b.Fatalf("Failed to create LSP client %d: %v", i, err)
		}
		clients = append(clients, client)
	}
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()

	collector := NewCollectorWithMultipleClients(javaTestCase, clients)
	collector.Language = uniast.Java
	collector.SetConcurrencyConfig(3, true, 10)

	result, err := collector.ScannerByTreeSitter(context.Background())
	if err != nil {
		b.Fatalf("Concurrent processing failed: %v", err)
	}
	println(result)

}

func TestCollector_Collect(t *testing.T) {
	log.SetLogLevel(log.DebugLevel)
	rustLSP, rustTestCase, err := lsp.InitLSPForFirstTest(uniast.Rust, "rust-analyzer")
	if err != nil {
		t.Fatalf("Failed to initialize rust LSP client: %v", err)
	}
	defer rustLSP.Close()

	t.Run("rustCollect", func(t *testing.T) {
		c := NewCollector(rustTestCase, rustLSP)
		c.LoadExternalSymbol = true
		err := c.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collector.Collect() failed = %v\n", err)
		}

		outdir := testutils.MakeTmpTestdir(true)
		marshals := []struct {
			val  any
			name string
		}{
			{&c.syms, "symbols"},
			{&c.deps, "deps"},
			{&c.funcs, "funcs"},
			{&c.vars, "vars"},
			{&c.repo, "repo"},
		}
		for _, m := range marshals {
			js, err := json.Marshal(m.val)
			if err != nil {
				t.Fatalf("Marshal %s failed: %v", m.name, err)
			}
			if err := os.WriteFile(outdir+"/"+m.name+".json", js, 0644); err != nil {
				t.Fatalf("Write json %s failed: %v", m.name, err)
			}
		}
	})
}
