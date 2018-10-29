// +build js,wasm

package cfworker

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall/js"

	"github.com/davecgh/go-spew/spew"
)

var (
	jsenvSetup sync.Mutex
	hassetup   bool
)

type jsHTTPResponse struct {
	code int
	h    http.Header
	buf  bytes.Buffer
}

func (j *jsHTTPResponse) WriteHeader(code int) {
	j.code = code
}

func (j *jsHTTPResponse) Write(p []byte) (int, error) {
	return j.buf.Write(p)
}

func (j *jsHTTPResponse) Header() http.Header {
	return j.h
}

// streamReader implements an io.ReadCloser wrapper for ReadableStream.
// See https://fetch.spec.whatwg.org/#readablestream for more information.
//
// copy-pasted from here: https://github.com/golang/go/blob/1399b52dc4a3cf5347603bf7011984cf28a34031/src/net/http/roundtrip_js.go#L175
type streamReader struct {
	pending []byte
	stream  js.Value
	err     error // sticky read error
}

var errClosed = errors.New("http reader is closed")

func (r *streamReader) Read(p []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(r.pending) == 0 {
		var (
			bCh   = make(chan []byte, 1)
			errCh = make(chan error, 1)
		)
		success := js.NewCallback(func(args []js.Value) {
			result := args[0]
			if result.Get("done").Bool() {
				errCh <- io.EOF
				return
			}
			value := make([]byte, result.Get("value").Get("byteLength").Int())
			a := js.TypedArrayOf(value)
			a.Call("set", result.Get("value"))
			a.Release()
			bCh <- value
		})
		defer success.Release()
		failure := js.NewCallback(func(args []js.Value) {
			// Assumes it's a TypeError. See
			// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/TypeError
			// for more information on this type. See
			// https://streams.spec.whatwg.org/#byob-reader-read for the spec on
			// the read method.
			errCh <- errors.New(args[0].Get("message").String())
		})
		defer failure.Release()
		r.stream.Call("read").Call("then", success, failure)
		select {
		case b := <-bCh:
			r.pending = b
		case err := <-errCh:
			r.err = err
			return 0, err
		}
	}
	n = copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *streamReader) Close() error {
	// This ignores any error returned from cancel method. So far, I did not encounter any concrete
	// situation where reporting the error is meaningful. Most users ignore error from resp.Body.Close().
	// If there's a need to report error here, it can be implemented and tested when that need comes up.
	r.stream.Call("cancel")
	if r.err == nil {
		r.err = errClosed
	}
	return nil
}

func assertExclusivity() {
	jsenvSetup.Lock()
	defer jsenvSetup.Unlock()
	if hassetup {
		panic("multiple cfworker.ListenAndServe calls")
	}
}

// ListenAndServe accepts a standard http.Handler and will set up the JS environment
// in a Cloudflare Worker. Once set up, the function also facilitates the translation of
// JS to Go HTTP object translation and then back the other way in order to respond correctly.
//
// BUG(nkcmr): When JS Header objects are passed into the WASM VM, there is some off-by-one error where one header will always be missing.
func ListenAndServe(h http.Handler) {
	assertExclusivity()
	c := make(chan struct{})
	goworkerdispatch := func(request, resolve, reject js.Value) {
		consoleLog("goworkerdispatch received request")
		goreqmethod := request.Get("method").String()
		gourl := request.Get("url").String()
		goheaders := http.Header{}
		jsIterate(request.Get("headers").Call("entries"), func(pair js.Value) {
			goheaders.Add(pair.Index(0).String(), pair.Index(1).String())
		})
		var body io.ReadCloser = http.NoBody
		b := request.Get("body")
		consoleLog(b, request)
		if b != js.Undefined() && b != js.Null() {
			body = &streamReader{stream: b.Call("getReader")}
		}
		spew.Dump(body)
		goreq, err := http.NewRequest(goreqmethod, gourl, body)
		if err != nil {
			reject.Invoke(js.Global().Get("Error").New(js.ValueOf(err.Error())))
			return
		}
		w := &jsHTTPResponse{
			h:    http.Header{},
			code: http.StatusOK,
		}
		h.ServeHTTP(w, goreq)
		resinit := js.Global().Get("Object").New()
		resinit.Set("status", js.ValueOf(w.code))
		resinit.Set("statusText", js.ValueOf(http.StatusText(w.code)))
		resinit.Set("headers", goHeaders2jsHeaders(w.h))
		jsres := js.Global().Get("Response").New(js.ValueOf(w.buf.String()), resinit)
		resolve.Invoke(jsres)
	}
	js.Global().Set("cf_go_wasm_handler", js.NewCallback(func(args []js.Value) {
		goworkerdispatch(args[0], args[1], args[2])
	}))
	eval(`global.addEventListener('fetch',function(a){a.respondWith(new Promise(function(b,c){cf_go_wasm_handler(a.request,b,c);}))});`)
	<-c // hang forever
}

func goHeaders2jsHeaders(goh http.Header) js.Value {
	jsh := js.Global().Get("Headers").New()
	for hk, hv := range goh {
		jsh.Call("append", hk, strings.Join(hv, "; "))
	}
	return jsh
}

func consoleLog(args ...interface{}) {
	js.Global().Get("console").Call("log", args...)
}

func eval(program string) js.Value {
	return js.Global().Call("eval", program)
}

func jsIterate(i js.Value, cb func(js.Value)) {
	for {
		result := i.Call("next")
		if result.Get("done").Bool() {
			return
		}
		cb(result.Get("value"))
	}
}
