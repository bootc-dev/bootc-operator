// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

const configPath = "/apis/node.bootc.dev/v1alpha1/bootcoperatorconfigs"

func TestLoad(t *testing.T) {
	resource := schema.GroupResource{Group: "node.bootc.dev", Resource: "bootcoperatorconfigs"}
	for _, tt := range []struct {
		name      string
		status    *apierrors.StatusError
		body      string
		code      int
		wantNil   bool
		wantError string
	}{
		{name: "one custom-named object"},
		{name: "no objects", body: `{"items":[]}`, wantNil: true},
		{
			name: "multiple objects", body: `{"items":[{"metadata":{"name":"z-config"}},` +
				`{"metadata":{"name":"a-config"}}]}`,
			wantError: "a-config, z-config); keep only one",
		},
		{
			name: "missing CRD", status: apierrors.NewNotFound(resource, ""),
			wantError: "ensure the bootcoperatorconfigs CRD is installed",
		},
		{
			name: "plain HTTP 404", code: 404, body: "not found",
			wantError: "ensure the bootcoperatorconfigs CRD is installed",
		},
		{
			name: "forbidden", status: apierrors.NewForbidden(resource, "", errors.New("access denied")),
			wantError: "this service account can list bootcoperatorconfigs",
		},
		{
			name: "unauthorized", status: apierrors.NewUnauthorized("authentication failed"),
			wantError: "authentication failed",
		},
		{name: "malformed response", body: `{`, wantError: "unexpected end"},
		{
			name: "zero daemon period", body: `{"items":[{"metadata":{"name":"custom-config"},` +
				`"spec":{"daemon":{"statusPollPeriodSeconds":0}}}]}`,
			wantError: "BootcOperatorConfig/custom-config spec.daemon.statusPollPeriodSeconds must be at least 1",
		},
		{
			name: "zero controller period", body: `{"items":[{"metadata":{"name":"custom-config"},` +
				`"spec":{"controller":{"tagResolutionPeriodSeconds":0}}}]}`,
			wantError: "BootcOperatorConfig/custom-config spec.controller.tagResolutionPeriodSeconds must be at least 1",
		},
		{
			name: "negative daemon period", body: `{"items":[{"metadata":{"name":"custom-config"},` +
				`"spec":{"daemon":{"statusPollPeriodSeconds":-1}}}]}`,
			wantError: "spec.daemon.statusPollPeriodSeconds must be at least 1",
		},
		{name: "unnamed object", body: `{"items":[{"spec":{}}]}`, wantError: "object without a name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			var resourceRequests []string
			wrapped := false
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				g.Expect(req.Method).To(Equal(http.MethodGet))
				g.Expect(req.URL.Path).To(Equal(configPath), "list the resource without discovery")
				g.Expect(req.URL.Query().Has("timeout")).To(BeTrue())
				g.Expect(req.URL.Query()).To(HaveLen(1), "list without a name or field selector")
				resourceRequests = append(resourceRequests, req.URL.Path)
				if tt.status != nil {
					tt.status.ErrStatus.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
					body, err := json.Marshal(tt.status.ErrStatus)
					g.Expect(err).NotTo(HaveOccurred())
					return jsonResponse(int(tt.status.ErrStatus.Code), string(body)), nil
				}
				body := tt.body
				if body == "" {
					body = `{"items":[{"metadata":{"name":"custom-config","uid":"original",` +
						`"generation":3},"spec":{"daemon":{"statusPollPeriodSeconds":17}}}]}`
				}
				code := tt.code
				if code == 0 {
					code = http.StatusOK
				}
				return jsonResponse(code, body), nil
			})
			restConfig := &rest.Config{
				Host:      "https://api.example.test",
				Transport: transport,
				Timeout:   time.Minute,
			}
			restConfig.Wrap(
				func(rt http.RoundTripper) http.RoundTripper { wrapped = true; return rt },
			)
			cfg, err := Load(context.Background(), restConfig, configScheme(t))
			g.Expect(restConfig.Timeout).To(Equal(time.Minute))
			g.Expect(wrapped).To(BeTrue(), "preserve existing transport wrappers")
			g.Expect(resourceRequests).To(Equal([]string{configPath}))
			if tt.wantError != "" {
				g.Expect(err).To(MatchError(ContainSubstring(tt.wantError)))
				g.Expect(cfg).To(BeNil())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			if tt.wantNil {
				g.Expect(cfg).To(BeNil())
				return
			}
			g.Expect(cfg.Name).To(Equal("custom-config"))
			g.Expect(cfg.UID).To(BeEquivalentTo("original"))
			g.Expect(cfg.Generation).To(Equal(int64(3)))
			g.Expect(*cfg.Spec.Daemon.StatusPollPeriodSeconds).To(Equal(int32(17)))
		})
	}
}

func TestLoadRequestFailures(t *testing.T) {
	for _, tt := range []struct{ failure, wantError string }{
		{"timeout", "deadline exceeded"},
		{"cancel", "context canceled"},
		{"connection", "connection refused"},
	} {
		t.Run(tt.failure, func(t *testing.T) {
			g := NewWithT(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := false
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				g.Expect(req.Method).To(Equal(http.MethodGet))
				g.Expect(req.URL.Path).To(Equal(configPath))
				injected = true
				if tt.failure == "connection" {
					return nil, errors.New("connection refused")
				}
				if tt.failure == "cancel" {
					cancel()
				}
				<-req.Context().Done()
				return nil, req.Context().Err()
			})
			cfg, err := load(
				ctx,
				&rest.Config{Host: "https://api.example.test", Transport: transport},
				configScheme(t),
				20*time.Millisecond,
			)
			g.Expect(injected).To(BeTrue())
			g.Expect(cfg).To(BeNil())
			g.Expect(err).To(MatchError(ContainSubstring(tt.wantError)))
		})
	}
}

func configScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	NewWithT(t).Expect(bootcv1alpha1.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func jsonResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
