// Copyright 2021 Polar Signals Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/conprof/symbol"
	"github.com/go-kit/kit/log/level"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/polarsignals/polarsignals-agent/ksym"
	"github.com/polarsignals/polarsignals-agent/template"
)

type flags struct {
	LogLevel           string   `enum:"error,warn,info,debug" help:"Log level." default:"info"`
	HttpAddress        string   `help:"Address to bind HTTP server to." default:":8080"`
	Node               string   `required help:"Name node the process is running on. If on Kubernetes, this must match the Kubernetes node name."`
	StoreAddress       string   `help:"gRPC address to send profiles and symbols to."`
	BearerToken        string   `help:"Bearer token to authenticate with store."`
	BearerTokenFile    string   `help:"File to read bearer token from to authenticate with store."`
	Insecure           bool     `help:"Send gRPC requests via plaintext instead of TLS."`
	InsecureSkipVerify bool     `help:"Skip TLS certificate verification."`
	SamplingRatio      float64  `help:"Sampling ratio to control how many of the discovered targets to profile. Defaults to 1.0, which is all." default:"1.0"`
	Kubernetes         bool     `help:"Discover containers running on this node to profile automatically."`
	PodLabelSelector   string   `help:"Label selector to control which Kubernetes Pods to select."`
	SystemdUnits       []string `help:"SystemD units to profile on this node."`
}

func main() {
	flags := flags{}
	kong.Parse(&flags)

	node := flags.Node
	logger := NewLogger(flags.LogLevel, LogFormatLogfmt, "")
	logger.Log("msg", "starting...", "node", node, "store", flags.StoreAddress)
	level.Debug(logger).Log("msg", "configuration", "bearertoken", flags.BearerToken, "insecure", flags.Insecure, "podselector", flags.PodLabelSelector, "samplingratio", flags.SamplingRatio)
	mux := http.NewServeMux()
	reg := prometheus.NewRegistry()
	ctx := context.Background()
	var g run.Group

	var (
		err error
		wc  storepb.WritableProfileStoreClient = NewNoopWritableProfileStoreClient()
		sc  SymbolStoreClient                  = NewNoopSymbolStoreClient()
	)

	if len(flags.StoreAddress) > 0 {
		conn, err := grpcConn(reg, flags)
		if err != nil {
			level.Error(logger).Log("err", err)
			os.Exit(1)
		}

		wc = storepb.NewWritableProfileStoreClient(conn)
		sc = symbol.NewSymbolStoreClient(storepb.NewSymbolStoreClient(conn))
	}

	ksymCache := ksym.NewKsymCache(logger)

	var (
		pm            *PodManager
		sm            *SystemdManager
		targetSources = []TargetSource{}
	)

	if flags.Kubernetes {
		pm, err = NewPodManager(
			logger,
			node,
			flags.PodLabelSelector,
			flags.SamplingRatio,
			ksymCache,
			wc,
			sc,
		)
		if err != nil {
			level.Error(logger).Log("err", err)
			os.Exit(1)
		}
		targetSources = append(targetSources, pm)
	}

	if len(flags.SystemdUnits) > 0 {
		sm = NewSystemdManager(
			logger,
			node,
			flags.SystemdUnits,
			flags.SamplingRatio,
			ksymCache,
			wc,
			sc,
		)
		if err != nil {
			level.Error(logger).Log("err", err)
			os.Exit(1)
		}
		targetSources = append(targetSources, sm)
	}

	m := NewTargetManager(targetSources)

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			activeProfilers := m.ActiveProfilers()

			statusPage := template.StatusPage{}
			for _, activeProfiler := range activeProfilers {
				profileType := ""
				labelSet := labels.Labels{}
				for _, label := range activeProfiler.Labels() {
					if label.Name == "__name__" {
						profileType = label.Value
					}
					if label.Name != "__name__" {
						labelSet = append(labelSet, labels.Label{Name: label.Name, Value: label.Value})
					}
				}
				sort.Sort(labelSet)

				q := url.Values{}
				q.Add("debug", "1")
				q.Add("query", labelSet.String())

				statusPage.ActiveProfilers = append(statusPage.ActiveProfilers, template.ActiveProfiler{
					Type:         profileType,
					Labels:       labelSet,
					LastTakenAgo: time.Now().Sub(activeProfiler.LastProfileTakenAt()),
					Error:        activeProfiler.LastError(),
					Link:         fmt.Sprintf("/active-profilers?%s", q.Encode()),
				})
			}

			sort.Slice(statusPage.ActiveProfilers, func(j, k int) bool {
				a := statusPage.ActiveProfilers[j].Labels
				b := statusPage.ActiveProfilers[k].Labels

				l := len(a)
				if len(b) < l {
					l = len(b)
				}

				for i := 0; i < l; i++ {
					if a[i].Name != b[i].Name {
						if a[i].Name < b[i].Name {
							return true
						}
						return false
					}
					if a[i].Value != b[i].Value {
						if a[i].Value < b[i].Value {
							return true
						}
						return false
					}
				}
				// If all labels so far were in common, the set with fewer labels comes first.
				return len(a)-len(b) < 0
			})

			err := template.StatusPageTemplate.Execute(w, statusPage)
			if err != nil {
				http.Error(w, "Unexpected error occurred while rendering status page: "+err.Error(), http.StatusInternalServerError)
			}

			return
		}
		if strings.HasPrefix(r.URL.Path, "/active-profilers") {
			ctx := r.Context()
			query := r.URL.Query().Get("query")
			matchers, err := parser.ParseMetricSelector(query)
			if err != nil {
				http.Error(w, `query incorrectly formatted, expecting selector in form of: {name1="value1",name2="value2"}`, http.StatusBadRequest)
				return
			}

			// We profile every 10 seconds so leaving 1s wiggle room. If after
			// 11s no profile has matched, then there is very likely no
			// profiler running that matches the label-set.
			ctx, _ = context.WithTimeout(ctx, time.Second*11)
			profile, err := m.NextMatchingProfile(ctx, matchers)
			if profile == nil || err == context.Canceled {
				http.Error(w, "No profile taken in the last 11 seconds that matches the requested label-matchers query. Profiles are taken every 10 seconds so either the profiler matching the label-set has stopped profiling, or the label-set was incorrect.", http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, "Unexpected error occurred: "+err.Error(), http.StatusInternalServerError)
				return
			}

			v := r.URL.Query().Get("debug")
			if v == "1" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				q := url.Values{}
				q.Add("query", query)

				fmt.Fprintf(w, "<p><a href='/active-profilers?%s'>Download Pprof</a></p>\n", q.Encode())
				fmt.Fprint(w, "<code><pre>\n")
				fmt.Fprint(w, profile.String())
				fmt.Fprint(w, "\n</pre></code>")
				return
			}

			w.Header().Set("Content-Type", "application/vnd.google.protobuf+gzip")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment;filename=%s.pb.gz", query))
			err = profile.Write(w)
			if err != nil {
				level.Error(logger).Log("msg", "failed to write profile", "err", err)
			}
			return
		}
		http.NotFound(w, r)
	})

	if len(flags.SystemdUnits) > 0 {
		ctx, cancel := context.WithCancel(ctx)
		g.Add(func() error {
			return sm.Run(ctx)
		}, func(error) {
			cancel()
		})
	}

	if flags.Kubernetes {
		ctx, cancel := context.WithCancel(ctx)
		g.Add(func() error {
			return pm.Run(ctx)
		}, func(error) {
			cancel()
		})
	}

	{
		ln, err := net.Listen("tcp", flags.HttpAddress)
		if err != nil {
			level.Error(logger).Log("err", err)
			return
		}
		g.Add(func() error {
			return http.Serve(ln, mux)
		}, func(error) {
			ln.Close()
		})
	}

	g.Add(run.SignalHandler(ctx, os.Interrupt, os.Kill))
	if err := g.Run(); err != nil {
		level.Error(logger).Log("err", err)
	}
}

func grpcConn(reg prometheus.Registerer, flags flags) (*grpc.ClientConn, error) {
	met := grpc_prometheus.NewClientMetrics()
	met.EnableClientHandlingTimeHistogram()
	reg.MustRegister(met)

	opts := []grpc.DialOption{
		grpc.WithUnaryInterceptor(
			met.UnaryClientInterceptor(),
		),
	}
	if flags.Insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		config := &tls.Config{
			InsecureSkipVerify: flags.InsecureSkipVerify,
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	}

	if flags.BearerToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
			token:    flags.BearerToken,
			insecure: flags.Insecure,
		}))
	}

	if flags.BearerTokenFile != "" {
		b, err := ioutil.ReadFile(flags.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read bearer token from file: %w", err)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(&perRequestBearerToken{
			token:    string(b),
			insecure: flags.Insecure,
		}))
	}

	return grpc.Dial(flags.StoreAddress, opts...)
}

type perRequestBearerToken struct {
	token    string
	insecure bool
}

func (t *perRequestBearerToken) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (t *perRequestBearerToken) RequireTransportSecurity() bool {
	return !t.insecure
}