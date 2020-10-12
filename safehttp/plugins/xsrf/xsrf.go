// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package xsrf provides a safehttp.Interceptor that ensures Cross-Site Request
// Forgery protection by verifying the incoming requests, rejecting those
// requests that are suspected to be part of an attack.
package xsrf

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/google/go-safeweb/safehttp"
	"golang.org/x/net/xsrftoken"
)

var statePreservingMethods = map[string]bool{
	safehttp.MethodGet:     true,
	safehttp.MethodHead:    true,
	safehttp.MethodOptions: true,
}

type Interceptor struct {
	secretAppKey string
	g            Generator
	c            Checker
	i            Injector
}

var _ safehttp.Interceptor = &Interceptor{}

func New(key string, g Generator, c Checker, i Injector) Interceptor {
	return Interceptor{
		secretAppKey: key,
		g:            g,
		c:            c,
		i:            i,
	}
}

func Default(key string) Interceptor {
	return Interceptor{
		secretAppKey: key,
		g: defaultGenerator{
			secretAppKey: key,
			cookieIDKey:  "xsrf-cookie",
		},
		c: defaultChecker{
			secretAppKey: key,
			cookieIDKey:  "xsrf-cookie",
			tokenKey:     "xsrf-token",
		},
		i: defaultInjector{},
	}
}

func Angular(cookieName, headerName string) Interceptor {
	return Interceptor{
		g: angularGenerator{tokenCookieName: cookieName},
		c: angularChecker{
			tokenCookieName: cookieName,
			tokenHeaderName: headerName,
		},
		i: angularInjector{},
	}
}

func (it *Interceptor) Before(w *safehttp.ResponseWriter, r *safehttp.IncomingRequest, _ safehttp.InterceptorConfig) safehttp.Result {
	code := it.c.Check(r)
	if code != safehttp.StatusOK {
		return w.WriteError(code)
	}
	return safehttp.NotWritten()
}

func (it *Interceptor) Commit(w *safehttp.ResponseWriter, r *safehttp.IncomingRequest, resp safehttp.Response, _ safehttp.InterceptorConfig) safehttp.Result {
	data, err := it.g.Generate(r)
	if err != nil {
		return w.WriteError(safehttp.StatusInternalServerError)
	}
	err = it.i.Inject(resp, w, data)
	if err != nil {
		return w.WriteError(safehttp.StatusInternalServerError)
	}
	return safehttp.Result{}
}

type Checker interface {
	Check(r *safehttp.IncomingRequest) safehttp.StatusCode
}

type Generator interface {
	Generate(r *safehttp.IncomingRequest) (GeneratedData, error)
}

type Injector interface {
	Inject(resp safehttp.Response, w *safehttp.ResponseWriter, data GeneratedData) error
}

type GeneratedData interface{}

type defaultGenerator struct {
	secretAppKey string
	cookieIDKey  string
}
type defaultData struct {
	cookieID  *safehttp.Cookie
	token     string
	setCookie bool
}

func (g defaultGenerator) Generate(r *safehttp.IncomingRequest) (GeneratedData, error) {
	data := defaultData{}
	c, err := r.Cookie(g.cookieIDKey)
	if err != nil {
		buf := make([]byte, 20)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("crypto/rand.Read: %v", err)
		}
		c = safehttp.NewCookie(g.cookieIDKey, base64.StdEncoding.EncodeToString(buf))
		c.SetSameSite(safehttp.SameSiteStrictMode)
		data.setCookie = true
	}
	data.cookieID = c
	tok := xsrftoken.Generate(g.secretAppKey, c.Value(), r.URL.Path())
	data.token = tok
	return data, nil
}

type defaultChecker struct {
	secretAppKey string
	cookieIDKey  string
	tokenKey     string
}

func (c defaultChecker) Check(r *safehttp.IncomingRequest) safehttp.StatusCode {
	if statePreservingMethods[r.Method()] {
		return safehttp.StatusOK
	}

	cookie, err := r.Cookie(c.cookieIDKey)
	if err != nil {
		return safehttp.StatusForbidden
	}

	f, err := r.PostForm()
	if err != nil {
		// We fallback to checking whether the form is multipart. Both types
		// are valid in an incoming request as long as the XSRF token is
		// present.
		mf, err := r.MultipartForm(32 << 20)
		if err != nil {
			return safehttp.StatusBadRequest
		}
		f = &mf.Form
	}

	tok := f.String(c.tokenKey, "")
	if f.Err() != nil || tok == "" {
		return safehttp.StatusUnauthorized
	}

	if !xsrftoken.Valid(tok, c.secretAppKey, cookie.Value(), r.URL.Path()) {
		return safehttp.StatusForbidden
	}
	return safehttp.StatusOK
}

type defaultInjector struct{}

func (i defaultInjector) Inject(resp safehttp.Response, w *safehttp.ResponseWriter, data GeneratedData) error {
	d, ok := data.(defaultData)
	if !ok {
		return errors.New("invalid data received")
	}
	if d.setCookie {
		if err := w.SetCookie(d.cookieID); err != nil {
			return err
		}
	}
	tmplResp, ok := resp.(safehttp.TemplateResponse)
	if !ok {
		return nil
	}
	// TODO(maramihali@): Change the key when function names are exported by
	// htmlinject
	// TODO: what should happen if the XSRFToken key is not present in the
	// tr.FuncMap?
	tmplResp.FuncMap["XSRFToken"] = func() string { return d.token }
	return nil
}

type angularGenerator struct {
	tokenCookieName string
}

type angularData struct {
	tokCookie *safehttp.Cookie
	setCookie bool
}

func (g angularGenerator) Generate(r *safehttp.IncomingRequest) (GeneratedData, error) {
	data := angularData{}
	c, err := r.Cookie(g.tokenCookieName)
	if err != nil {
		tok := make([]byte, 20)
		if _, err := rand.Read(tok); err != nil {
			return nil, fmt.Errorf("crypto/rand.Read: %v", err)
		}
		c = safehttp.NewCookie(g.tokenCookieName, base64.StdEncoding.EncodeToString(tok))

		c.SetSameSite(safehttp.SameSiteStrictMode)
		c.SetPath("/")
		// Set the duration of the token cookie to 24 hours.
		c.SetMaxAge(86400)
		// Needed in order to make the cookie accessible by JavaScript
		// running on the user's domain.
		c.DisableHTTPOnly()
		data.setCookie = true
	}
	data.tokCookie = c
	return data, nil
}

type angularChecker struct {
	tokenCookieName string
	tokenHeaderName string
}

func (c angularChecker) Check(r *safehttp.IncomingRequest) safehttp.StatusCode {
	if statePreservingMethods[r.Method()] {
		return safehttp.StatusOK
	}
	cookie, err := r.Cookie(c.tokenCookieName)
	if err != nil {
		return safehttp.StatusForbidden
	}
	tok := r.Header.Get(c.tokenHeaderName)
	if tok == "" || tok != cookie.Value() {
		return safehttp.StatusUnauthorized
	}
	return safehttp.StatusOK
}

type angularInjector struct{}

func (i angularInjector) Inject(resp safehttp.Response, w *safehttp.ResponseWriter, data GeneratedData) error {
	d, ok := data.(angularData)
	if !ok {
		return errors.New("invalid data received")
	}
	if d.setCookie {
		if err := w.SetCookie(d.tokCookie); err != nil {
			return err
		}
	}
	return nil
}
