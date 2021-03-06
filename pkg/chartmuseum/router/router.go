/*
Copyright The Helm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"fmt"
	"regexp"

	cm_logger "github.com/helm/chartmuseum/pkg/chartmuseum/logger"

	"github.com/gin-contrib/size"
	"github.com/gin-gonic/gin"
	"github.com/zsais/go-gin-prometheus"
)

type (
	// Router handles all incoming HTTP requests
	Router struct {
		*gin.Engine
		Logger           *cm_logger.Logger
		Routes           []*Route
		TlsCert          string
		TlsKey           string
		ContextPath      string
		BasicAuthHeader  string
		BearerAuthHeader string
		AnonymousGet     bool
		Depth            int
		AuthType         string
		AuthRealm        string
		AuthService      string
		AuthIssuer       string
		AuthPublicCert   []byte
	}

	// RouterOptions are options for constructing a Router
	RouterOptions struct {
		Logger        *cm_logger.Logger
		Username      string
		Password      string
		ContextPath   string
		TlsCert       string
		TlsKey        string
		PathPrefix    string
		EnableMetrics bool
		AnonymousGet  bool
		Depth         int
		MaxUploadSize int
		BearerAuth    bool
		AuthType      string
		AuthRealm     string
		AuthService   string
		AuthIssuer    string
		AuthCertPath  string
	}

	// Route represents an application route
	Route struct {
		Method  string
		Path    string
		Handler gin.HandlerFunc
		Action  action
	}

	action string
)

var (
	RepoPullAction   action = "pull"
	RepoPushAction   action = "push"
	SystemInfoAction action = "sysinfo"
)

// NewRouter creates a new Router instance
func NewRouter(options RouterOptions) *Router {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(requestWrapper(options.Logger))
	engine.Use(limits.RequestSizeLimiter(int64(options.MaxUploadSize)))

	if options.EnableMetrics {
		p := ginprometheus.NewPrometheus("chartmuseum")
		p.ReqCntURLLabelMappingFn = mapURLWithParamsBackToRouteTemplate
		p.Use(engine)
	}

	router := &Router{
		Engine:       engine,
		Routes:       []*Route{},
		Logger:       options.Logger,
		TlsCert:      options.TlsCert,
		TlsKey:       options.TlsKey,
		ContextPath:  options.ContextPath,
		AnonymousGet: options.AnonymousGet,
		Depth:        options.Depth,
	}

	// if BearerAuth is true, looks for required inputs.
	// example input:
	// --bearer-auth=true
	// --auth-realm="https://127.0.0.1:5001/auth" 
	// --auth-service="chartmuseum" 
	// --auth-issuer="Acme auth server"
	// --auth-cert-path="./certs/authorization-server-cert.pem"
	if options.BearerAuth {
		if options.AuthRealm == "" {
			router.Logger.Fatal("Missing Auth Realm")
		}
		if options.AuthService == "" {
			router.Logger.Fatal("Missing Auth Service")
		}
		if options.AuthIssuer == "" {
			router.Logger.Fatal("Missing Auth Issuer")
		}
		if options.AuthCertPath == "" {
			router.Logger.Fatal("Missing Auth Server Public Cert Path")
		}
		if options.AuthType != "token" {
			router.Logger.Fatal("Invalid auth type: only accept token auth")
		}
		router.AuthType = options.AuthType
		router.AuthRealm = options.AuthRealm
		router.AuthService = options.AuthService
		router.AuthIssuer = options.AuthIssuer

		// loads certificate from file
		loadPublicCertFromFile(options.AuthCertPath, router)

		router.BearerAuthHeader = "Bearer"
	}

	if options.Username != "" && options.Password != "" {
		router.BasicAuthHeader = generateBasicAuthHeader(options.Username, options.Password)
	}

	router.NoRoute(router.masterHandler)

	return router
}

func (router *Router) Start(port int) {
	router.Logger.Infow("Starting ChartMuseum",
		"port", port,
	)
	if router.TlsCert != "" && router.TlsKey != "" {
		router.Logger.Fatal(router.RunTLS(fmt.Sprintf(":%d", port), router.TlsCert, router.TlsKey))
	} else {
		router.Logger.Fatal(router.Run(fmt.Sprintf(":%d", port)))
	}
}

// SetRoutes applies list of routes
func (router *Router) SetRoutes(routes []*Route) {
	router.Routes = routes
}

// all incoming requests are passed through this handler
func (router *Router) masterHandler(c *gin.Context) {
	route, params := match(router.Routes, c.Request.Method, c.Request.URL.Path, router.ContextPath, router.Depth)
	if route == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.Params = params

	if isRepoAction(route.Action) {


		authorized, responseHeaders := router.authorizeRequest(c.Request)
		for key, value := range responseHeaders {
			c.Header(key, value)
		}
		if !authorized {
			c.JSON(401, gin.H{"error": "unauthorized"})
			return
		}
	}

	route.Handler(c)
}

/*
mapURLWithParamsBackToRouteTemplate is a valid ginprometheus ReqCntURLLabelMappingFn.
For every route containing parameters (e.g. `/charts/:filename`, `/api/charts/:name/:version`, etc)
the actual parameter values will be replaced by their name, to minimize the cardinality of the
`chartmuseum_requests_total{url=..}` Prometheus counter.
*/
func mapURLWithParamsBackToRouteTemplate(c *gin.Context) string {
	url := c.Request.URL.String()
	for _, p := range c.Params {
		re := regexp.MustCompile(fmt.Sprintf(`(^.*?)/\b%s\b(.*$)`, regexp.QuoteMeta(p.Value)))
		url = re.ReplaceAllString(url, fmt.Sprintf(`$1/:%s$2`, p.Key))
	}
	return url
}
