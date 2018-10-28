// +build js,wasm

package cfworker

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"syscall/js"
)

var jsenvSetup sync.Once

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

// ListenAndServe accepts a standard http.Handler and will set up the JS environment
// in a Cloudflare Worker. Once set up, the function also facilitates the translation of
// JS to Go HTTP object translation and then back the other way in order to respond correctly.
//
// BUG(nkcmr): When JS Header objects are passed into the WASM VM, there is some off-by-one error where one header will always be missing.
func ListenAndServe(h http.Handler) {
	c := make(chan struct{})
	goworkerdispatch := func(request, resolve, reject js.Value) {
		consoleLog("goworkerdispatch received request")
		goreqmethod := request.Get("method").String()
		gourl := request.Get("url").String()
		goheaders := http.Header{}
		jsIterate(request.Get("headers").Call("entries"), func(pair js.Value) {
			goheaders.Add(pair.Index(0).String(), pair.Index(1).String())
		})
		goreq, err := http.NewRequest(goreqmethod, gourl, http.NoBody)
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
	jsenvSetup.Do(func() {
		js.Global().Set("cf_go_wasm_handler", js.NewCallback(func(args []js.Value) {
			goworkerdispatch(args[0], args[1], args[2])
		}))
		js.Global().Call("eval", `addEventListener('fetch',function(a){a.respondWith(new Promise(function(b,c){global.cf_go_wasm_handler(a.request,b,c);}))});`)
	})
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
