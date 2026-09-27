package routeros

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file locks the REST contract (verb, path, auth, body, decoding,
// error handling) against a fake router. It is separate from client_test.go,
// which covers base-URL construction and the optional port.

type recordedRequest struct {
	Method      string
	Path        string
	User        string
	Pass        string
	AuthOK      bool
	Body        string
	Accept      string
	ContentType string
}

type recorder struct {
	mu   sync.Mutex
	recs []recordedRequest
}

func (r *recorder) add(rec recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
}

func (r *recorder) last(t *testing.T) recordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recs) == 0 {
		t.Fatal("no requests recorded")
	}
	return r.recs[len(r.recs)-1]
}

// newRestClient returns a Client pointed at an httptest server. It builds the
// struct field-by-field because New hardcodes https://<host>[:port]/rest;
// here we need the server's ephemeral http://127.0.0.1:<port>/rest base.
func newRestClient(t *testing.T, handler http.Handler) (*Client, *recorder) {
	t.Helper()

	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		user, pass, ok := r.BasicAuth()
		rec.add(recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			User:        user,
			Pass:        pass,
			AuthOK:      ok,
			Body:        string(body),
			Accept:      r.Header.Get("Accept"),
			ContentType: r.Header.Get("Content-Type"),
		})
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	c := &Client{
		baseURL:  srv.URL + "/rest",
		user:     "mtha",
		password: "s3cret",
		http:     srv.Client(),
	}
	return c, rec
}

func TestNewBuildsBaseURLAndDefaultsTimeout(t *testing.T) {
	c := New(Config{Host: "10.0.0.2", User: "mtha"})

	if c.baseURL != "https://10.0.0.2/rest" {
		t.Errorf("baseURL = %q, want https://10.0.0.2/rest", c.baseURL)
	}
	if c.http.Timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", c.http.Timeout)
	}
}

func TestGetSendsBasicAuthAndDecodes(t *testing.T) {
	c, rec := newRestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"core-a"}`)
	}))

	var out Identity
	if err := c.Get(context.Background(), "/system/identity", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Name != "core-a" {
		t.Errorf("decoded name = %q, want core-a", out.Name)
	}

	got := rec.last(t)
	if got.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.Method)
	}
	if got.Path != "/rest/system/identity" {
		t.Errorf("path = %q, want /rest/system/identity", got.Path)
	}
	if !got.AuthOK || got.User != "mtha" || got.Pass != "s3cret" {
		t.Errorf("auth = %q/%q ok=%v, want mtha/s3cret", got.User, got.Pass, got.AuthOK)
	}
	if got.Accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", got.Accept)
	}
}

func TestWriteMethodsMapToRESTVerbs(t *testing.T) {
	cases := []struct {
		name string
		call func(*Client) error
		want string
	}{
		{
			name: "Post creates via PUT",
			call: func(c *Client) error {
				return c.Post(context.Background(), "/system/script", map[string]string{"name": "x"}, nil)
			},
			want: http.MethodPut,
		},
		{
			name: "Patch updates",
			call: func(c *Client) error {
				return c.Patch(context.Background(), "/ip/service/*1", map[string]string{"port": "22"}, nil)
			},
			want: http.MethodPatch,
		},
		{
			name: "Command runs via POST",
			call: func(c *Client) error {
				return c.Command(context.Background(), "/system/backup/save", map[string]string{"name": "x"}, nil)
			},
			want: http.MethodPost,
		},
		{
			name: "Delete removes",
			call: func(c *Client) error { return c.Delete(context.Background(), "/system/script/*1") },
			want: http.MethodDelete,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newRestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := rec.last(t).Method; got != tc.want {
				t.Errorf("method = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestWriteSendsJSONBodyAndContentType(t *testing.T) {
	c, rec := newRestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ret":"*1"}`)
	}))

	if err := c.Post(context.Background(), "/ip/dns/static", map[string]string{"name": "host1", "address": "10.0.0.5"}, nil); err != nil {
		t.Fatalf("Post: %v", err)
	}

	got := rec.last(t)
	if got.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.ContentType)
	}
	if got.Body != `{"address":"10.0.0.5","name":"host1"}` {
		t.Errorf("body = %q", got.Body)
	}
}

func TestGetWithoutBodyOmitsContentType(t *testing.T) {
	c, rec := newRestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))

	if err := c.Get(context.Background(), "/system/identity", &Identity{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ct := rec.last(t).ContentType; ct != "" {
		t.Errorf("Content-Type = %q, want empty on a GET", ct)
	}
}

func TestErrorStatusIsReturnedWithCodeAndBody(t *testing.T) {
	c, _ := newRestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":400,"message":"bad request"}`)
	}))

	err := c.Get(context.Background(), "/system/resource", nil)
	if err == nil {
		t.Fatal("expected an error for status 400")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad request") {
		t.Errorf("error %q should mention the status code and response body", err)
	}
}

// The payloads below follow docs/lab-rest-contract.md (RouterOS 7.23.7),
// except netwatch, which was empty on the lab for the whole survey: its
// entry shape is hypothetical until the harness populates one.
func TestTypedReadersDecodeRouterOSPayloads(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/system/resource", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"version":"7.23.7 (long-term)","board-name":"RB5009","uptime":"1d2h3m","cpu-load":"3","free-memory":"512000000","total-memory":"1073741824"}`)
	})
	mux.HandleFunc("/rest/system/identity", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"core-a"}`)
	})
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, vrrpMasterPayload)
	})
	mux.HandleFunc("/rest/tool/netwatch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{".id":"*1","host":"1.1.1.1","status":"up","comment":"mtha: probe"}]`)
	})

	c, _ := newRestClient(t, mux)
	ctx := context.Background()

	res, err := c.SystemResource(ctx)
	if err != nil {
		t.Fatalf("SystemResource: %v", err)
	}
	if res.Version != "7.23.7 (long-term)" || res.BoardName != "RB5009" || res.CPULoad != "3" {
		t.Errorf("SystemResource = %+v", res)
	}

	id, err := c.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Name != "core-a" {
		t.Errorf("Identity.Name = %q, want core-a", id.Name)
	}

	vrrp, err := c.VRRP(ctx)
	if err != nil {
		t.Fatalf("VRRP: %v", err)
	}
	if len(vrrp) != 1 || vrrp[0].Role() != RoleMaster || vrrp[0].Priority != "200" {
		t.Errorf("VRRP = %+v", vrrp)
	}
	if vrrp[0].ID != "*4" {
		t.Errorf("VRRP[0].ID = %q, want *4 decoded from the \".id\" field", vrrp[0].ID)
	}

	nw, err := c.Netwatch(ctx)
	if err != nil {
		t.Fatalf("Netwatch: %v", err)
	}
	if len(nw) != 1 || nw[0].Host != "1.1.1.1" || nw[0].Status != "up" {
		t.Errorf("Netwatch = %+v", nw)
	}
	if nw[0].ID != "*1" {
		t.Errorf("Netwatch[0].ID = %q, want *1 decoded from the \".id\" field", nw[0].ID)
	}
}

// vrrpMasterPayload and vrrpBackupPayload are GET /rest/interface/vrrp as
// the lab pair returns it (docs/lab-rest-contract.md): exactly one role flag,
// always "true", with the other key absent rather than "false". Every value
// is a string. The instance's other always-present configuration fields
// (interval, version, arp, on-master, ...) are left out; nothing decodes
// them.
const (
	vrrpMasterPayload = `[{".id":"*4","name":"vrrp-lan","interface":"ether2","vrid":"1","priority":"200","password":"","on-master":"","on-backup":"","on-fail":"","disabled":"false","invalid":"false","running":"true","master":"true"}]`
	vrrpBackupPayload = `[{".id":"*4","name":"vrrp-lan","interface":"ether2","vrid":"1","priority":"100","password":"","on-master":"","on-backup":"","on-fail":"","disabled":"false","invalid":"false","running":"false","backup":"true"}]`
)

// The backup is the payload the master check has to decode positively: a
// backup's VRRP interface is not running, so running:"false" must not read
// as a fault or an unknown role.
func TestVRRPDecodesDevicePayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    VRRPRole
	}{
		{"master", vrrpMasterPayload, RoleMaster},
		{"backup", vrrpBackupPayload, RoleBackup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.payload)
			})
			c, _ := newRestClient(t, mux)

			vrrp, err := c.VRRP(context.Background())
			if err != nil {
				t.Fatalf("VRRP: %v", err)
			}
			if len(vrrp) != 1 {
				t.Fatalf("got %d instances, want 1", len(vrrp))
			}
			if got := vrrp[0].Role(); got != tc.want {
				t.Errorf("role = %v, want %v (%+v)", got, tc.want, vrrp[0])
			}
			if vrrp[0].ID != "*4" || vrrp[0].Name != "vrrp-lan" || vrrp[0].Interface != "ether2" {
				t.Errorf("instance = %+v", vrrp[0])
			}
		})
	}
}

// Deliberately hypothetical, not captured from a device: RouterOS 7.23.7
// never sends "vrrp-state", never sends a role flag as "false", and never
// sends a JSON boolean. These shapes keep the defensive decoding paths
// (Flag's JSON-boolean case, the vrrp-state fallback in Role) covered so a
// payload that does use them still decodes, and anything not positively
// placed stays unknown.
func TestVRRPDecodesHypotheticalPayloadShapes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/interface/vrrp", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
			{".id":"*1","name":"vrrp-lan","interface":"ether2","priority":"200","master":"true","backup":"false","running":"true","disabled":"false"},
			{".id":"*2","name":"vrrp-wan","interface":"ether1","priority":"100","vrrp-state":"backup"},
			{".id":"*3","name":"vrrp-mgmt","interface":"ether3","priority":"100","master":false,"backup":false,"running":"false","disabled":"true"}
		]`)
	})
	c, _ := newRestClient(t, mux)

	vrrp, err := c.VRRP(context.Background())
	if err != nil {
		t.Fatalf("VRRP: %v", err)
	}
	want := []VRRPRole{RoleMaster, RoleBackup, RoleUnknown}
	if len(vrrp) != len(want) {
		t.Fatalf("got %d instances, want %d", len(vrrp), len(want))
	}
	for i, w := range want {
		if got := vrrp[i].Role(); got != w {
			t.Errorf("%s role = %v, want %v (%+v)", vrrp[i].Name, got, w, vrrp[i])
		}
	}
}

func TestVRRPRole(t *testing.T) {
	cases := []struct {
		name string
		v    VRRPInstance
		want VRRPRole
	}{
		// The shapes the device sends: one flag, "true", the other absent.
		{"master flag", VRRPInstance{Master: "true"}, RoleMaster},
		{"backup flag", VRRPInstance{Backup: "true"}, RoleBackup},
		// Neither flag (a disabled or init instance, say) could not be
		// observed read-only; unknown is the safe answer either way.
		{"nothing reported", VRRPInstance{}, RoleUnknown},

		// Defensive: shapes RouterOS 7.23.7 has not been seen to send.
		{"master flag with explicit false", VRRPInstance{Master: "true", Backup: "false"}, RoleMaster},
		{"backup flag with explicit false", VRRPInstance{Master: "false", Backup: "true"}, RoleBackup},
		{"neither flag set", VRRPInstance{Master: "false", Backup: "false"}, RoleUnknown},
		{"both flags set", VRRPInstance{Master: "true", Backup: "true"}, RoleUnknown},

		// Defensive: the vrrp-state fallback. No such field exists on
		// RouterOS 7.23.7.
		{"vrrp-state master", VRRPInstance{State: "master"}, RoleMaster},
		{"vrrp-state backup", VRRPInstance{State: "backup"}, RoleBackup},
		{"flags and state agree", VRRPInstance{Backup: "true", State: "backup"}, RoleBackup},
		{"flags and state disagree", VRRPInstance{Backup: "true", State: "master"}, RoleUnknown},
		{"other state", VRRPInstance{State: "init"}, RoleUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Role(); got != tc.want {
				t.Errorf("Role() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ip/service as the lab returns it: static built-ins (telnet is *0, a real
// id), dynamic service rows, and one dynamic reverse-proxy row per open HTTPS
// connection — including the request reading it — whose .id and remote
// change on every request. The name reverse-proxy appears twice. Field sets
// follow the contract; the values are illustrative, as the contract's are.
const ipServicePayload = `[
	{".id":"*0","name":"telnet","port":"23","proto":"tcp","address":"","disabled":"true","dynamic":"false","invalid":"false","max-sessions":"20","vrf":"main"},
	{".id":"*2","name":"ssh","port":"22","proto":"tcp","address":"","disabled":"false","dynamic":"false","invalid":"false","max-sessions":"20","vrf":"main"},
	{".id":"*4","name":"www-ssl","port":"443","proto":"tcp","address":"","disabled":"false","dynamic":"false","invalid":"false","max-sessions":"20","vrf":"main","certificate":"none","tls-version":"any"},
	{".id":"*9","name":"reverse-proxy","port":"443","proto":"tcp","address":"","disabled":"false","dynamic":"false","invalid":"false","max-sessions":"20","vrf":"main","certificate":"none","tls-version":"any"},
	{".id":"*B","name":"btest","port":"2000","proto":"tcp","disabled":"false","dynamic":"true","invalid":"false"},
	{".id":"*1A","name":"reverse-proxy","port":"443","proto":"tcp","disabled":"false","dynamic":"true","invalid":"false","connection":"true","local":"172.17.0.2:443","remote":"172.17.0.1:51234"}
]`

func TestGetSectionReturnsRawEntries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ip/service", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, ipServicePayload)
	})

	c, rec := newRestClient(t, mux)

	entries, err := c.GetSection(context.Background(), "ip/service")
	if err != nil {
		t.Fatalf("GetSection: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6", len(entries))
	}
	// GetSection must return entries untouched; dropping .id/dynamic is the
	// model package's job, not the client's. That includes the dynamic rows
	// and the *0 id.
	if entries[0][".id"] != "*0" || entries[0]["name"] != "telnet" {
		t.Errorf("*0 entry not returned raw: %+v", entries[0])
	}
	if last := entries[5]; last[".id"] != "*1A" || last["dynamic"] != "true" || last["remote"] != "172.17.0.1:51234" {
		t.Errorf("dynamic connection row not returned raw: %+v", last)
	}
	if got := rec.last(t).Path; got != "/rest/ip/service" {
		t.Errorf("path = %q, want /rest/ip/service", got)
	}
}

// ip/route ids come in all three shapes the device uses, and GetSection
// hands them through as strings: *2018xxxx for a connected route and
// *80000001 for the DHCP-client default route. Only dynamic routes were
// observed on the lab; a static route's shape is not known yet. Values are
// illustrative.
func TestGetSectionKeepsRouteIDShapes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/ip/route", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[
			{".id":"*80000001","dst-address":"0.0.0.0/0","gateway":"172.17.0.1","immediate-gw":"172.17.0.1%ether1","distance":"1","scope":"30","target-scope":"10","routing-table":"main","vrf-interface":"ether1","dynamic":"true","active":"true","inactive":"false","dhcp":"true"},
			{".id":"*20183040","dst-address":"192.168.88.0/24","gateway":"ether2","immediate-gw":"ether2","local-address":"192.168.88.2%ether2","distance":"0","scope":"10","target-scope":"5","routing-table":"main","dynamic":"true","active":"true","inactive":"false","connect":"true","ecmp":"true"}
		]`)
	})
	c, _ := newRestClient(t, mux)

	entries, err := c.GetSection(context.Background(), "ip/route")
	if err != nil {
		t.Fatalf("GetSection: %v", err)
	}
	if len(entries) != 2 || entries[0][".id"] != "*80000001" || entries[1][".id"] != "*20183040" {
		t.Errorf("route ids not returned raw: %+v", entries)
	}
}
