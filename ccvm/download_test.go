/*
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pkg/errors"
)

func startTestHTTPServer(wg *sync.WaitGroup) (*http.Server, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", errors.Wrap(err, "Unable to create listener")
	}

	mux := http.NewServeMux()

	/* We're going to make our test files 11 MB here.  This figure is large
	enough to allow us to test cancelling. */

	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		zeros := make([]byte, 1024)
		for i := 0; i < 1024*11; i++ {
			_, _ = w.Write(zeros)
		}
	})

	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	server := &http.Server{
		Handler: mux,
	}
	wg.Add(1)
	go func() {
		_ = server.Serve(listener)
		_ = listener.Close()
		wg.Done()
	}()

	return server, listener.Addr().String(), nil
}

func testDownloadSingle(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	path, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
		"http://"+addr+"/download/one", func(bool, progress) {})
	if err != nil {
		t.Errorf("Failed to download file : %v", err)
	}
	_, err = os.Stat(path)
	if err != nil {
		t.Errorf("Unable to stat downloaded file")
	}
}

func testDownloadDouble(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	pathChans := []chan string{make(chan string), make(chan string)}

	for _, ch := range pathChans {
		go func(ch chan string) {
			path, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
				"http://"+addr+"/download/double", func(bool, progress) {})
			if err != nil {
				t.Errorf("Failed to download file : %v", err)
			}
			ch <- path
		}(ch)
	}

	path1 := <-pathChans[0]
	path2 := <-pathChans[1]

	if path1 != path2 {
		t.Errorf("Paths of downloaded files do not match: %s != %s", path1, path2)
	}
	_, err := os.Stat(path1)
	if err != nil {
		t.Errorf("Unable to stat downloaded file")
	}
}

func testDownloadDoubleDifferent(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	pathChans := []chan string{make(chan string), make(chan string)}

	for i, ch := range pathChans {
		go func(ch chan string, i int) {
			url := fmt.Sprintf("http://%s/download/double-%d", addr, i)
			path, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
				url, func(bool, progress) {})
			if err != nil {
				t.Errorf("Failed to download file : %v", err)
			}
			ch <- path
		}(ch, i)
	}

	path1 := <-pathChans[0]
	path2 := <-pathChans[1]

	_, err := os.Stat(path1)
	if err != nil {
		t.Errorf("Unable to stat downloaded file")
	}

	_, err = os.Stat(path2)
	if err != nil {
		t.Errorf("Unable to stat downloaded file")
	}
}

func testDownloadCancelSingle(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	type dld struct {
		path string
		err  error
	}

	ch := make(chan dld)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	go func() {
		path, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
			"http://"+addr+"/download/singlecancel", func(bool, progress) {})
		ch <- dld{path, err}
	}()
	res := <-ch
	if res.err == nil {
		_, err := os.Stat(res.path)
		if err != nil {
			t.Errorf("Unable to stat downloaded file")
		}
	}
}

func testDownloadCancelOneOfTwo(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	type dld struct {
		path string
		err  error
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	params := []struct {
		ch  chan dld
		ctx context.Context
	}{
		{make(chan dld), ctx},
		{make(chan dld), cancelCtx},
	}

	for _, p := range params {
		go func(ctx context.Context, ch chan dld) {
			path, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
				"http://"+addr+"/download/oneoftwo", func(bool, progress) {})
			ch <- dld{path, err}
		}(p.ctx, p.ch)
	}

	cancel()
	ret1 := <-params[0].ch
	<-params[1].ch

	if ret1.err != nil {
		t.Errorf("Download failed : %v", ret1.err)
	} else {
		_, err := os.Stat(ret1.path)
		if err != nil {
			t.Errorf("Unable to stat downloaded file")
		}
	}
}

func testDownloadError(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	_, err := downloadFile(ctx, downloadCh, http.DefaultTransport.(*http.Transport),
		"http://"+addr+"/error", func(bool, progress) {})
	if err == nil {
		t.Errorf("Expected download to fail")
	}
}

// Logically, this function belongs in ccvm_test.go.  However, we've got all the
// infrastructure necessary in download_test.go to test downloads, so it's easier
// to simply include the test here.
func testDownloadImages(ctx context.Context, t *testing.T, downloadCh chan<- downloadRequest, addr, ccvmDir string) {
	wkld := &workload{
		spec: workloadSpec{
			BaseImageURL: "http://" + addr + "/download/image",
			BIOS:         "http://" + addr + "/download/bios",
		},
	}

	resultCh := make(chan interface{})
	go func() {
		img, bios, err := downloadImages(ctx, wkld, http.DefaultTransport.(*http.Transport),
			resultCh, downloadCh)
		if err != nil {
			t.Errorf("Failed to download images: %v", err)
		}
		if len(img) == 0 || len(bios) == 0 {
			t.Errorf("One the paths is empty img=%s bios=%s", img, bios)
		}
		close(resultCh)
	}()

	for range resultCh {
	}

	wkld.spec.BIOS = "ftp://" + addr + "/download/bios"
	resultCh = make(chan interface{})
	go func() {
		_, _, err := downloadImages(ctx, wkld, http.DefaultTransport.(*http.Transport),
			resultCh, downloadCh)
		if err == nil {
			t.Errorf("Expected downloadImages with bad BIOS URL to fail")
		}
		close(resultCh)
	}()

	for range resultCh {
	}
}

func TestDownload(t *testing.T) {
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	ccvmDir, err := ioutil.TempDir("", "ccloudvm-download-")
	if err != nil {
		t.Fatalf("Failed to create temporary directory")
	}
	defer func() {
		_ = os.RemoveAll(ccvmDir)
	}()

	d := downloader{}
	downloadCh := make(chan downloadRequest)
	err = d.setup(ccvmDir)
	if err != nil {
		t.Fatalf("Unable to start download manager")
	}

	wg.Add(1)
	go func() {
		d.start(ctx.Done(), downloadCh)
		wg.Done()
	}()

	server, addr, err := startTestHTTPServer(&wg)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("single", func(t *testing.T) {
		testDownloadSingle(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("single-repeat", func(t *testing.T) {
		testDownloadSingle(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("double", func(t *testing.T) {
		testDownloadDouble(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("cancelsingle", func(t *testing.T) {
		testDownloadCancelSingle(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("canceloneoftwo", func(t *testing.T) {
		testDownloadCancelOneOfTwo(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("error", func(t *testing.T) {
		testDownloadError(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("doubledifferent", func(t *testing.T) {
		testDownloadDoubleDifferent(ctx, t, downloadCh, addr, ccvmDir)
	})
	t.Run("downloadImages", func(t *testing.T) {
		testDownloadImages(ctx, t, downloadCh, addr, ccvmDir)
	})
	cancel()
	_ = server.Shutdown(context.Background())
	wg.Wait()
}
