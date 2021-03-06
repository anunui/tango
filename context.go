// Copyright 2015 The Tango Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tango

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type Handler interface {
	Handle(*Context)
}

type Context struct {
	tan *Tango
	Logger

	idx int
	req *http.Request
	ResponseWriter
	route    *Route
	params   Params
	callArgs []reflect.Value
	matched  bool
	stage    byte

	action interface{}
	Result interface{}
}

func (ctx *Context) reset(req *http.Request, resp ResponseWriter) {
	ctx.req = req
	ctx.ResponseWriter = resp
	ctx.idx = 0
	ctx.stage = 0
	ctx.route = nil
	ctx.params = nil
	ctx.callArgs = nil
	ctx.matched = false
	ctx.action = nil
	ctx.Result = nil
}

func (ctx *Context) HandleError() {
	ctx.tan.ErrHandler.Handle(ctx)
}

func (ctx *Context) Req() *http.Request {
	return ctx.req
}

func (ctx *Context) IsAjax() bool {
	return ctx.Req().Header.Get("X-Requested-With") == "XMLHttpRequest"
}

func (ctx *Context) SecureCookies(secret string) Cookies {
	return &secureCookies{
		(*cookies)(ctx),
		secret,
	}
}

func (ctx *Context) Cookies() Cookies {
	return (*cookies)(ctx)
}

func (ctx *Context) Forms() *Forms {
	ctx.req.ParseForm()
	return (*Forms)(ctx.req)
}

func (ctx *Context) Route() *Route {
	ctx.newAction()
	return ctx.route
}

func (ctx *Context) Params() *Params {
	ctx.newAction()
	return &ctx.params
}

func (ctx *Context) IP() string {
	proxy := []string{}
	if ips := ctx.Req().Header.Get("X-Forwarded-For"); ips != "" {
		proxy = strings.Split(ips, ",")
	}
	if len(proxy) > 0 && proxy[0] != "" {
		return proxy[0]
	}
	ip := strings.Split(ctx.Req().RemoteAddr, ":")
	if len(ip) > 0 {
		if ip[0] != "[" {
			return ip[0]
		}
	}
	return "127.0.0.1"
}

func (ctx *Context) Action() interface{} {
	ctx.newAction()
	return ctx.action
}

func (ctx *Context) newAction() {
	if !ctx.matched {
		reqPath := removeStick(ctx.Req().URL.Path)
		ctx.route, ctx.params = ctx.tan.Match(reqPath, ctx.Req().Method)
		if ctx.route != nil {
			vc := ctx.route.newAction()
			ctx.action = vc.Interface()
			switch ctx.route.routeType {
			case StructPtrRoute:
				ctx.callArgs = []reflect.Value{vc.Elem()}
			case StructRoute:
				ctx.callArgs = []reflect.Value{vc}
			case FuncRoute:
				ctx.callArgs = []reflect.Value{}
			case FuncHttpRoute:
				ctx.callArgs = []reflect.Value{reflect.ValueOf(ctx.ResponseWriter),
					reflect.ValueOf(ctx.Req())}
			case FuncReqRoute:
				ctx.callArgs = []reflect.Value{reflect.ValueOf(ctx.Req())}
			case FuncResponseRoute:
				ctx.callArgs = []reflect.Value{reflect.ValueOf(ctx.ResponseWriter)}
			case FuncCtxRoute:
				ctx.callArgs = []reflect.Value{reflect.ValueOf(ctx)}
			default:
				panic("routeType error")
			}
		}
		ctx.matched = true
	}
}

// WARNING: don't invoke this method on action
func (ctx *Context) Next() {
	ctx.idx += 1
	ctx.invoke()
}

func (ctx *Context) execute() {
	ctx.newAction()
	// route is matched
	if ctx.action != nil {
		if len(ctx.route.handlers) > 0 && ctx.stage == 0 {
			ctx.idx = 0
			ctx.stage = 1
			ctx.invoke()
			return
		}

		var ret []reflect.Value
		switch fn := ctx.route.raw.(type) {
		case func(*Context):
			fn(ctx)
		case func(*http.Request, http.ResponseWriter):
			fn(ctx.req, ctx.ResponseWriter)
		case func():
			fn()
		case func(*http.Request):
			fn(ctx.req)
		case func(http.ResponseWriter):
			fn(ctx.ResponseWriter)
		default:
			ret = ctx.route.method.Call(ctx.callArgs)
		}

		if len(ret) == 1 {
			ctx.Result = ret[0].Interface()
		} else if len(ret) == 2 {
			if code, ok := ret[0].Interface().(int); ok {
				ctx.Result = &StatusResult{code, ret[1].Interface()}
			}
		}
		// not route matched
	} else {
		if !ctx.Written() {
			ctx.NotFound()
		}
	}
}

func (ctx *Context) invoke() {
	if ctx.stage == 0 {
		if ctx.idx < len(ctx.tan.handlers) {
			ctx.tan.handlers[ctx.idx].Handle(ctx)
		} else {
			ctx.execute()
		}
	} else if ctx.stage == 1 {
		if ctx.idx < len(ctx.route.handlers) {
			ctx.route.handlers[ctx.idx].Handle(ctx)
		} else {
			ctx.execute()
		}
	}
}

func (ctx *Context) ServeFile(path string) error {
	http.ServeFile(ctx, ctx.Req(), path)
	return nil
}

func (ctx *Context) ServeXml(obj interface{}) error {
	encoder := xml.NewEncoder(ctx)
	ctx.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	err := encoder.Encode(obj)
	if err != nil {
		ctx.Header().Del("Content-Type")
	}
	return err
}

func (ctx *Context) ServeJson(obj interface{}) error {
	encoder := json.NewEncoder(ctx)
	ctx.Header().Set("Content-Type", "application/json; charset=UTF-8")
	err := encoder.Encode(obj)
	if err != nil {
		ctx.Header().Del("Content-Type")
	}
	return err
}

func (ctx *Context) Body() ([]byte, error) {
	body, err := ioutil.ReadAll(ctx.req.Body)
	if err != nil {
		return nil, err
	}

	ctx.req.Body.Close()
	ctx.req.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	return body, nil
}

func (ctx *Context) DecodeJson(obj interface{}) error {
	body, err := ctx.Body()
	if err != nil {
		return err
	}

	return json.Unmarshal(body, obj)
}

func (ctx *Context) DecodeXml(obj interface{}) error {
	body, err := ctx.Body()
	if err != nil {
		return err
	}

	return xml.Unmarshal(body, obj)
}

func (ctx *Context) Download(fpath string) error {
	f, err := os.Open(fpath)
	if err != nil {
		return err
	}
	defer f.Close()

	fName := filepath.Base(fpath)
	ctx.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%v\"", fName))
	_, err = io.Copy(ctx, f)
	return err
}

func (ctx *Context) SaveToFile(formName, savePath string) error {
	file, _, err := ctx.Req().FormFile(formName)
	if err != nil {
		return err
	}
	defer file.Close()

	f, err := os.OpenFile(savePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, file)
	return err
}

func (ctx *Context) Redirect(url string, status ...int) {
	s := http.StatusFound
	if len(status) > 0 {
		s = status[0]
	}
	http.Redirect(ctx.ResponseWriter, ctx.Req(), url, s)
}

// Notmodified writes a 304 HTTP response
func (ctx *Context) NotModified() {
	ctx.WriteHeader(http.StatusNotModified)
}

func (ctx *Context) Unauthorized() {
	ctx.Abort(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
}

// NotFound writes a 404 HTTP response
func (ctx *Context) NotFound(message ...string) {
	if len(message) == 0 {
		ctx.Abort(http.StatusNotFound, http.StatusText(http.StatusNotFound))
		return
	}
	ctx.Abort(http.StatusNotFound, message[0])
}

// Abort is a helper method that sends an HTTP header and an optional
// body. It is useful for returning 4xx or 5xx errors.
// Once it has been called, any return value from the handler will
// not be written to the response.
func (ctx *Context) Abort(status int, body string) {
	ctx.Result = Abort(status, body)
	ctx.HandleError()
}

type Contexter interface {
	SetContext(*Context)
}

type Ctx struct {
	*Context
}

func (c *Ctx) SetContext(ctx *Context) {
	c.Context = ctx
}

func Contexts() HandlerFunc {
	return func(ctx *Context) {
		if action := ctx.Action(); action != nil {
			if a, ok := action.(Contexter); ok {
				a.SetContext(ctx)
			}
		}
		ctx.Next()
	}
}
