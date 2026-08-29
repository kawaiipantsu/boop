package tools

import (
	"context"
	"fmt"
	"github.com/kawaiipantsu/boop/internal/webclient"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// execLocalHTTPTool returns a tool allowed to reach the test server, which
// necessarily listens on loopback.
func execLocalHTTPTool() *HTTPTool {
	tool := NewHTTPTool()
	tool.AllowPrivateNetworks = true
	return tool
}

func execHTTPCall(t *testing.T, args execHTTPArgs) Call {
	t.Helper()
	return execTestCall(t, "http", args)
}

func TestHTTPToolSuccessfulRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=FAKE-TEST-COOKIE; Path=/")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"method":%q,"ua":%q}`, r.Method, r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	res, err := execLocalHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result:\n%s", res.Content)
	}
	for _, want := range []string{"GET " + srv.URL, "status: 200", "--- headers ---", "--- body ---", `"method":"GET"`, "--- end ---"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "FAKE-TEST-COOKIE") {
		t.Errorf("Set-Cookie must be redacted:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "[redacted]") {
		t.Errorf("expected a redaction marker:\n%s", res.Content)
	}
	data, ok := res.Data.(HTTPResponse)
	if !ok {
		t.Fatalf("Data = %T, want HTTPResponse", res.Data)
	}
	if data.StatusCode != 200 || data.BodyBytes == 0 {
		t.Errorf("unexpected structured data: %+v", data)
	}
	// The tool must identify itself, and must do so with the same agent
	// string every other outbound request uses, so a site operator sees one
	// recognisable client rather than several.
	if !strings.Contains(strings.ToLower(data.Body), "boop") {
		t.Errorf("the tool should identify itself with a User-Agent: %s", data.Body)
	}
	if !strings.Contains(data.Body, webclient.DefaultUserAgent()) {
		t.Errorf("User-Agent should be the shared default %q, got body %s",
			webclient.DefaultUserAgent(), data.Body)
	}
}

func TestHTTPToolSendsMethodHeadersAndBody(t *testing.T) {
	var gotMethod, gotBody, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod, gotBody, gotHeader = r.Method, string(body), r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	res, err := execLocalHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{
		Method:  "post",
		URL:     srv.URL + "/things",
		Headers: map[string]string{"X-Trace": "abc123"},
		Body:    `{"name":"boop"}`,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Errorf("201 must not be an error:\n%s", res.Content)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotBody != `{"name":"boop"}` {
		t.Errorf("body = %q", gotBody)
	}
	if gotHeader != "abc123" {
		t.Errorf("header = %q", gotHeader)
	}
}

func TestHTTPToolErrorStatusIsData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such widget", http.StatusNotFound)
	}))
	defer srv.Close()

	res, err := execLocalHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL + "/widget/9"}))
	if err != nil {
		t.Fatalf("a 404 must not be a Go error: %v", err)
	}
	if !res.IsError {
		t.Error("a 404 must be reported as an error result")
	}
	for _, want := range []string{"status: 404", "no such widget"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestHTTPToolBlocksLoopbackByDefault(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	res, err := NewHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("loopback must be refused by default:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "loopback") {
		t.Errorf("content should explain the refusal:\n%s", res.Content)
	}
	if reached {
		t.Error("the request must never reach the server")
	}
}

func TestHTTPToolBlocksMetadataEndpointEvenWhenLocalIsAllowed(t *testing.T) {
	tool := execLocalHTTPTool()
	res, err := tool.Execute(context.Background(), execHTTPCall(t, execHTTPArgs{
		URL: "http://169.254.169.254/latest/meta-data/iam/security-credentials/",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("the cloud metadata endpoint must always be refused")
	}
	if !strings.Contains(res.Content, "metadata") {
		t.Errorf("content should name the reason:\n%s", res.Content)
	}
}

func TestHTTPToolBlocksRedirectIntoMetadataEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	// Loopback is allowed here, so only the redirect target can stop this.
	res, err := execLocalHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a redirect into a blocked address must fail:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "SSRF protection") || !strings.Contains(res.Content, "169.254.169.254") {
		t.Errorf("content should name the blocked redirect target:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "Do not retry") {
		t.Errorf("the model should be told not to retry:\n%s", res.Content)
	}
}

func TestHTTPToolStrictModeRefusesPrivateDestinations(t *testing.T) {
	// The guard is re-applied to every hop, so a redirect can never launder a
	// blocked destination; TestHTTPToolBlocksRedirectIntoMetadataEndpoint
	// covers the redirect path, and this covers the direct one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the strict default must not reach a loopback server")
	}))
	defer srv.Close()

	res, err := NewHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "loopback") {
		t.Errorf("the strict default must refuse a private destination:\n%s", res.Content)
	}
}

func TestHTTPToolRedirectLimit(t *testing.T) {
	var srv *httptest.Server
	hops := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, srv.URL+fmt.Sprintf("/hop/%d", hops), http.StatusFound)
	}))
	defer srv.Close()

	tool := execLocalHTTPTool()
	tool.MaxRedirects = 2
	res, err := tool.Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("an endless redirect chain must fail")
	}
	if !strings.Contains(res.Content, "stopped after 2 redirects") {
		t.Errorf("content = %s", res.Content)
	}
	if hops > 4 {
		t.Errorf("followed %d hops, want the limit to stop it early", hops)
	}
}

func TestHTTPToolCapsResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	defer srv.Close()

	tool := execLocalHTTPTool()
	tool.MaxResponseBytes = 128
	res, err := tool.Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data := res.Data.(HTTPResponse)
	if data.BodyBytes != 128 {
		t.Errorf("body bytes = %d, want the 128-byte cap", data.BodyBytes)
	}
	if !data.Truncated {
		t.Error("truncation must be reported")
	}
	if !strings.Contains(res.Content, "[body truncated at 128 bytes]") {
		t.Errorf("content should say the body was truncated:\n%s", res.Content)
	}
}

func TestHTTPToolTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	tool := execLocalHTTPTool()
	tool.Timeout = 50 * time.Millisecond
	start := time.Now()
	res, err := tool.Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("a timeout must be an error result")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout was not enforced: took %s", elapsed)
	}
	if !strings.Contains(res.Content, "timed out") && !strings.Contains(res.Content, "deadline") {
		t.Errorf("content should explain the timeout:\n%s", res.Content)
	}
}

func TestHTTPToolBinaryBodyIsNotDumped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0x00, 0x01, 0x02, 0xff, 0xfe})
	}))
	defer srv.Close()

	res, err := execLocalHTTPTool().Execute(context.Background(), execHTTPCall(t, execHTTPArgs{URL: srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, "binary body") {
		t.Errorf("binary payloads must be described, not dumped:\n%q", res.Content)
	}
}

func TestHTTPToolRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name     string
		args     execHTTPArgs
		contains string
	}{
		{"missing url", execHTTPArgs{}, "url is required"},
		{"unsupported scheme", execHTTPArgs{URL: "file:///etc/passwd"}, "unsupported scheme"},
		{"unsupported method", execHTTPArgs{URL: "https://example.com", Method: "TRACE"}, "not supported"},
		{"no host", execHTTPArgs{URL: "http://"}, "no host"},
		{"localhost by name", execHTTPArgs{URL: "http://localhost:8080/x"}, "local hostname"},
		{"loopback literal", execHTTPArgs{URL: "http://127.0.0.1:9/x"}, "loopback"},
		{"private literal", execHTTPArgs{URL: "http://192.168.1.1/x"}, "private-range"},
		{"ipv6 loopback", execHTTPArgs{URL: "http://[::1]:9/x"}, "loopback"},
		{"mapped ipv4 loopback", execHTTPArgs{URL: "http://[::ffff:127.0.0.1]:9/x"}, "loopback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := NewHTTPTool().Execute(context.Background(), execHTTPCall(t, tc.args))
			if err != nil {
				t.Fatalf("Execute returned a Go error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected a refusal, got:\n%s", res.Content)
			}
			if !strings.Contains(res.Content, tc.contains) {
				t.Errorf("content = %q, want it to mention %q", res.Content, tc.contains)
			}
		})
	}
}

func TestHTTPToolBlockReason(t *testing.T) {
	strict := NewHTTPTool()
	permissive := execLocalHTTPTool()

	tests := []struct {
		ip            string
		blockedStrict bool
		blockedLocal  bool
	}{
		{"127.0.0.1", true, false},
		{"::1", true, false},
		{"0.0.0.0", true, false},
		{"10.1.2.3", true, false},
		{"172.16.0.1", true, false},
		{"192.168.0.5", true, false},
		{"169.254.10.1", true, false},
		{"fe80::1", true, false},
		{"fd00::1", true, false},
		{"100.64.0.1", true, false},
		{"198.18.0.1", true, false},
		{"224.0.0.1", true, false},
		{"169.254.169.254", true, true},
		{"169.254.170.2", true, true},
		{"fd00:ec2::254", true, true},
		{"100.100.100.200", true, true},
		{"93.184.216.34", false, false},
		{"2606:2800:220:1:248:1893:25c8:1946", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad fixture address %q", tc.ip)
			}
			if _, blocked := strict.execBlockReason(ip); blocked != tc.blockedStrict {
				t.Errorf("strict blocked = %v, want %v", blocked, tc.blockedStrict)
			}
			if _, blocked := permissive.execBlockReason(ip); blocked != tc.blockedLocal {
				t.Errorf("with the local opt-in blocked = %v, want %v", blocked, tc.blockedLocal)
			}
		})
	}
}

func TestHTTPToolDialControl(t *testing.T) {
	tool := NewHTTPTool()
	if err := tool.execDialControl("tcp", "127.0.0.1:80", nil); err == nil {
		t.Error("the dial guard must refuse loopback")
	}
	if err := tool.execDialControl("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public addresses must be allowed: %v", err)
	}
	if err := tool.execDialControl("tcp", "not-an-address", nil); err == nil {
		t.Error("an unparseable address must be refused rather than allowed")
	}
}

func TestHTTPToolPermission(t *testing.T) {
	tool := NewHTTPTool()
	tests := []struct {
		method string
		want   permissions.Risk
	}{
		{"", permissions.RiskLow},
		{"get", permissions.RiskLow},
		{"HEAD", permissions.RiskLow},
		{"POST", permissions.RiskMedium},
		{"PUT", permissions.RiskMedium},
		{"PATCH", permissions.RiskMedium},
		{"DELETE", permissions.RiskHigh},
		{"TRACE", permissions.RiskHigh},
	}
	for _, tc := range tests {
		t.Run("method "+tc.method, func(t *testing.T) {
			action, err := tool.Permission(execHTTPCall(t, execHTTPArgs{Method: tc.method, URL: "https://api.example.com/v1/things"}))
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if action.Category != permissions.CatNetworkHTTP {
				t.Errorf("category = %q, want network.http", action.Category)
			}
			if action.Risk != tc.want {
				t.Errorf("risk = %q, want %q", action.Risk, tc.want)
			}
			if !strings.Contains(action.Summary, "api.example.com") {
				t.Errorf("summary = %q", action.Summary)
			}
		})
	}
}

func TestHTTPToolPermissionRedactsCredentials(t *testing.T) {
	tool := NewHTTPTool()
	action, err := tool.Permission(execHTTPCall(t, execHTTPArgs{
		Method: "POST",
		URL:    "https://api.example.com/v1/things",
		Headers: map[string]string{
			"Authorization": "Bearer nobody-should-see-this",
			"X-Api-Key":     "also-secret",
			"Content-Type":  "application/json",
		},
		Body: `{"name":"boop"}`,
	}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if strings.Contains(action.Detail, "nobody-should-see-this") || strings.Contains(action.Detail, "also-secret") {
		t.Errorf("credentials must never appear in an approval prompt:\n%s", action.Detail)
	}
	if !strings.Contains(action.Detail, "Content-Type: application/json") {
		t.Errorf("ordinary headers should be visible:\n%s", action.Detail)
	}
	if !strings.Contains(action.Detail, `{"name":"boop"}`) {
		t.Errorf("the body should be previewed for approval:\n%s", action.Detail)
	}
}

func TestHTTPToolPermissionRequiresURL(t *testing.T) {
	if _, err := NewHTTPTool().Permission(execHTTPCall(t, execHTTPArgs{})); err == nil {
		t.Fatal("expected an error when no url is given")
	}
}

func TestHTTPToolImplementsTool(t *testing.T) {
	var tool Tool = NewHTTPTool()
	if tool.Name() != "http" {
		t.Errorf("name = %q", tool.Name())
	}
	props, ok := tool.Schema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	for _, key := range []string{"url", "method", "headers", "body", "timeout_seconds"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema is missing %q", key)
		}
	}
	if _, ok := props["allow_private_networks"]; ok {
		t.Error("the SSRF opt-in must not be exposed as a model-settable argument")
	}
}

func TestExecIsSensitiveHeader(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Authorization", true},
		{"authorization", true},
		{"Cookie", true},
		{"Set-Cookie", true},
		{"X-Api-Key", true},
		{"X-Auth-Token", true},
		{"X-Session-Secret", true},
		{"Content-Type", false},
		{"Accept", false},
	}
	for _, tc := range tests {
		if got := execIsSensitiveHeader(tc.name); got != tc.want {
			t.Errorf("execIsSensitiveHeader(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
