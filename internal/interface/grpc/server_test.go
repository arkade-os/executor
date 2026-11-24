package grpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	executorv1 "github.com/arkade-os/executor/api-spec/protobuf/gen/executor/v1"
	"github.com/arkade-os/executor/internal/interface/grpc/handlers"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestListen(t *testing.T) {
	tests := []struct {
		name           string
		publicRegistry bool
		public         codes.Code
		rest           int
	}{
		{
			name:   "the public port refuses the registry",
			public: codes.PermissionDenied, rest: http.StatusForbidden,
		},
		{
			name: "the public port registers when opened", publicRegistry: true,
			public: codes.InvalidArgument, rest: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Listen(
				t.Context(),
				Options{
					PublicRegistry: tt.publicRegistry, Status: statusStub{}, Health: &healthStub{},
				},
				&handlers.Executor{}, &handlers.Admin{},
			)
			require.NoError(t, err)
			t.Cleanup(s.Stop)
			go func() { _ = s.Serve() }()

			register := func(addr string) []codes.Code {
				conn, err := grpc.NewClient(
					addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				client := executorv1.NewExecutorServiceClient(conn)
				_, artifact := client.RegisterArtifact(
					t.Context(), &executorv1.RegisterArtifactRequest{},
				)
				_, template := client.RegisterTemplate(
					t.Context(), &executorv1.RegisterTemplateRequest{},
				)
				_, read := client.GetArtifact(t.Context(), &executorv1.GetArtifactRequest{})
				require.Equal(t, codes.InvalidArgument, status.Code(read), "reading is served")
				return []codes.Code{status.Code(artifact), status.Code(template)}
			}
			require.Equal(t, []codes.Code{tt.public, tt.public}, register(s.PublicAddr()))
			require.Equal(
				t, []codes.Code{codes.InvalidArgument, codes.InvalidArgument},
				register(s.AdminAddr()), "the admin port reaches the handler",
			)

			resp, err := http.Post(
				"http://"+s.PublicAddr()+"/v1/template", "application/json",
				strings.NewReader(`{}`),
			)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			require.Equal(t, tt.rest, resp.StatusCode)
		})
	}
}

func TestIsRest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		contentType string
		expected    bool
	}{
		{name: "get", method: http.MethodGet, expected: true},
		{name: "delete", method: http.MethodDelete, expected: true},
		{name: "json", method: http.MethodPost, contentType: "application/json", expected: true},
		{
			name: "json with a charset", method: http.MethodPost,
			contentType: "application/json; charset=utf-8", expected: true,
		},
		{name: "no content type", method: http.MethodPost},
		{name: "form", method: http.MethodPost, contentType: "text/plain"},
		{
			name: "json named in a parameter of a form type", method: http.MethodPost,
			contentType: "text/plain;x=application/json",
		},
		{name: "grpc", method: http.MethodPost, contentType: "application/grpc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/v1/admin/delegation/1/cancel", nil)
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			require.Equal(t, tc.expected, isRest(r))
		})
	}
}

func TestOnlyThePublicPortIsRateLimited(t *testing.T) {
	s, err := Listen(
		t.Context(), Options{PublicRateLimit: 1, Status: statusStub{}, Health: &healthStub{}},
		&handlers.Executor{}, &handlers.Admin{},
	)
	require.NoError(t, err)
	t.Cleanup(s.Stop)

	preflight := func(h http.Handler, remote string) int {
		req := httptest.NewRequest(http.MethodOptions, "/v1/info", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	limited := 0
	for range 30 {
		if preflight(s.public.http.Handler, "203.0.113.7:4000") == http.StatusTooManyRequests {
			limited++
		}
		require.Equal(t, http.StatusOK, preflight(s.admin.http.Handler, "203.0.113.7:4000"))
		require.Equal(t, http.StatusOK, preflight(s.public.http.Handler, "127.0.0.1:4000"))
	}
	require.InDelta(t, 20, limited, 2, "ten requests pass, the rest of the thirty do not")
}
