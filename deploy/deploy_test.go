// Package deploy pins what the deployment files promise (design: "Media:
// LiveKit, TURN, Egress" and "Deployment"): the Kubernetes base as
// `kubectl kustomize` renders it, the example overlay, and the Compose
// fragment. A test reads the RENDERED objects and the LiveKit config as
// LiveKit parses it (YAML), never the files' wording.
//
// kubectl is needed to render; without it the Kubernetes tests skip, unless
// MEETINGS_REQUIRE_KUBECTL=1 (CI and scripts/mutate set it), which makes them
// fail instead, so a sweep can never pass by skipping.
package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type obj = map[string]any

// render is `kubectl kustomize dir`, decoded.
func render(t *testing.T, dir string) []obj {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("MEETINGS_REQUIRE_KUBECTL") == "1" {
			t.Fatal("kubectl is not on PATH and MEETINGS_REQUIRE_KUBECTL=1")
		}
		t.Skip("kubectl is not on PATH (MEETINGS_REQUIRE_KUBECTL=1 makes this fail)")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(kubectl, "kustomize", dir)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	var docs []obj
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var o obj
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding %s: %v", dir, err)
		}
		if o != nil {
			docs = append(docs, o)
		}
	}
	return docs
}

// dig walks m by keys (string) and indexes (int); nil when absent.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(obj)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

func find(t *testing.T, docs []obj, kind, name string) obj {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && dig(d, "metadata", "name") == name {
			return d
		}
	}
	t.Fatalf("no %s %q rendered", kind, name)
	return nil
}

func ofKind(docs []obj, kind string) []obj {
	var out []obj
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

func container(t *testing.T, dep obj, name string) obj {
	t.Helper()
	cs, _ := dig(dep, "spec", "template", "spec", "containers").([]any)
	for _, c := range cs {
		if dig(c, "name") == name {
			return c.(obj)
		}
	}
	t.Fatalf("deployment %v has no container %q", dig(dep, "metadata", "name"), name)
	return nil
}

// envEntry is one container env entry and its position.
func envEntry(c obj, name string) (obj, int) {
	es, _ := c["env"].([]any)
	for i, e := range es {
		if dig(e, "name") == name {
			return e.(obj), i
		}
	}
	return nil, -1
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// The LiveKit server pinned by the index digest of 1.13.11, in both files.
var liveKitImage = regexp.MustCompile(`^docker\.io/livekit/livekit-server:v1\.13\.11@sha256:[0-9a-f]{64}$`)

// The webhook goes to the plugin's INTERNAL port, by its Service name.
const webhookURL = "http://muster-meetings:8081/livekit/webhook"

// The Service's NodePort range: LiveKit's own port ranges must stay out of it.
const nodePortLo, nodePortHi = 30000, 32767

// lkWant is what a LiveKit config's per-deployment values must be.
type lkWant struct{ domain, nodeIP, apiKey string }

// checkLiveKitConfig pins one LiveKit config (parsed as LiveKit parses it).
func checkLiveKitConfig(t *testing.T, where, body string, want lkWant) obj {
	t.Helper()
	var cfg obj
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("%s: the LiveKit config is not YAML: %v", where, err)
	}
	// room: the plugin alone makes rooms (2b audit), and the timeouts.
	if v, ok := dig(cfg, "room").(obj)["auto_create"]; !ok || v != false {
		t.Errorf("%s: room.auto_create = %v (set: %v), want an explicit false", where, v, ok)
	}
	for _, k := range []string{"empty_timeout", "departure_timeout"} {
		if n, _ := toInt(dig(cfg, "room", k)); n != 600 {
			t.Errorf("%s: room.%s = %v, want 600", where, k, dig(cfg, "room", k))
		}
	}
	// TURN: TLS on 5349 for the configured host, the mounted certificate,
	// and no TURN/UDP listener.
	if dig(cfg, "turn", "enabled") != true {
		t.Errorf("%s: turn.enabled = %v, want true", where, dig(cfg, "turn", "enabled"))
	}
	if n, _ := toInt(dig(cfg, "turn", "tls_port")); n != 5349 {
		t.Errorf("%s: turn.tls_port = %v, want 5349", where, dig(cfg, "turn", "tls_port"))
	}
	if n, _ := toInt(dig(cfg, "turn", "udp_port")); n != 0 {
		t.Errorf("%s: turn.udp_port = %d: TURN over UDP is open, want no UDP listener", where, n)
	}
	if got := dig(cfg, "turn", "domain"); got != want.domain {
		t.Errorf("%s: turn.domain = %v, want %q", where, got, want.domain)
	}
	if dig(cfg, "turn", "cert_file") != "/etc/livekit/turn-tls/tls.crt" || dig(cfg, "turn", "key_file") != "/etc/livekit/turn-tls/tls.key" {
		t.Errorf("%s: turn.cert_file, key_file = %v, %v; want the mounted Secret's tls.crt, tls.key", where, dig(cfg, "turn", "cert_file"), dig(cfg, "turn", "key_file"))
	}
	// rtc: node_ip from configuration, and the one private peer TURN may
	// relay to; no UDP mux; pinned interfaces.
	if got := dig(cfg, "rtc", "node_ip"); got != want.nodeIP {
		t.Errorf("%s: rtc.node_ip = %v, want %q", where, got, want.nodeIP)
	}
	if got := dig(cfg, "turn", "allow_restricted_peer_cidrs"); !reflect.DeepEqual(got, []any{want.nodeIP + "/32"}) {
		t.Errorf("%s: turn.allow_restricted_peer_cidrs = %v, want exactly [%s/32]", where, got, want.nodeIP)
	}
	if v := dig(cfg, "rtc", "udp_port"); v != nil {
		t.Errorf("%s: rtc.udp_port = %v, want unset (the pinned port range, no UDP mux)", where, v)
	}
	if inc, _ := dig(cfg, "rtc", "interfaces", "includes").([]any); len(inc) == 0 {
		t.Errorf("%s: rtc.interfaces.includes is empty, want it pinned", where)
	}
	// The two port ranges: pinned, outside the NodePort range, apart.
	type rng struct {
		name   string
		lo, hi int
	}
	var rs []rng
	for _, r := range [][3]string{{"rtc.port_range", "rtc", "port_range"}, {"turn.relay_range", "turn", "relay_range"}} {
		lo, ok1 := toInt(dig(cfg, r[1], r[2]+"_start"))
		hi, ok2 := toInt(dig(cfg, r[1], r[2]+"_end"))
		if !ok1 || !ok2 || lo <= 0 || hi < lo || hi > 65535 {
			t.Errorf("%s: %s = %v-%v, want a pinned range", where, r[0], dig(cfg, r[1], r[2]+"_start"), dig(cfg, r[1], r[2]+"_end"))
			continue
		}
		if lo <= nodePortHi && hi >= nodePortLo {
			t.Errorf("%s: %s %d-%d overlaps the NodePort range %d-%d", where, r[0], lo, hi, nodePortLo, nodePortHi)
		}
		for _, p := range []int{7880, 7881, 7882, 5349} {
			if p >= lo && p <= hi {
				t.Errorf("%s: %s %d-%d holds port %d", where, r[0], lo, hi, p)
			}
		}
		rs = append(rs, rng{r[0], lo, hi})
	}
	if len(rs) == 2 && rs[0].lo <= rs[1].hi && rs[1].lo <= rs[0].hi {
		t.Errorf("%s: %s and %s overlap", where, rs[0].name, rs[1].name)
	}
	// The webhook: the plugin's internal port, signed with the API key.
	if got := dig(cfg, "webhook", "urls"); !reflect.DeepEqual(got, []any{webhookURL}) {
		t.Errorf("%s: webhook.urls = %v, want exactly [%s]", where, got, webhookURL)
	}
	if got := dig(cfg, "webhook", "api_key"); got != want.apiKey {
		t.Errorf("%s: webhook.api_key = %v, want %q", where, got, want.apiKey)
	}
	// Secrets never in the config body.
	for _, k := range []string{"keys", "key_file"} {
		if v, ok := cfg[k]; ok {
			t.Errorf("%s: the config carries %s (%v): the keys come from LIVEKIT_KEYS", where, k, v)
		}
	}
	return cfg
}

var kubeVar = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)

// 🔴 LiveKit in Kubernetes: the pinned image, the config's invariants, and
// every $(NAME) it uses defined by an EARLIER env entry (the kubelet expands
// only those; a later or missing one stays literal text).
func TestKubernetesLiveKitConfig(t *testing.T) {
	docs := render(t, "kubernetes")
	dep := find(t, docs, "Deployment", "livekit")
	c := container(t, dep, "livekit")
	if img, _ := c["image"].(string); !liveKitImage.MatchString(img) {
		t.Errorf("livekit image = %q, want livekit-server v1.13.11 pinned by digest", img)
	}
	cfgEnv, _ := envEntry(c, "LIVEKIT_CONFIG")
	if cfgEnv == nil {
		t.Fatal("the livekit container has no LIVEKIT_CONFIG")
	}
	for _, name := range []string{"LIVEKIT_CONFIG", "LIVEKIT_KEYS"} {
		e, i := envEntry(c, name)
		v, _ := e["value"].(string)
		for _, m := range kubeVar.FindAllStringSubmatch(v, -1) {
			if _, j := envEntry(c, m[1]); j < 0 || j >= i {
				t.Errorf("%s uses $(%s), which is not defined before it", name, m[1])
			}
		}
	}
	body, _ := cfgEnv["value"].(string)
	checkLiveKitConfig(t, "kubernetes", body, lkWant{domain: "$(TURN_DOMAIN)", nodeIP: "$(LIVEKIT_NODE_IP)", apiKey: "$(LIVEKIT_API_KEY)"})
	if e, _ := envEntry(c, "TURN_DOMAIN"); dig(e, "valueFrom", "configMapKeyRef", "key") != "turn-domain" {
		t.Errorf("TURN_DOMAIN = %v, want the params ConfigMap's turn-domain", e)
	}
	if e, _ := envEntry(c, "LIVEKIT_NODE_IP"); dig(e, "valueFrom", "fieldRef", "fieldPath") != "status.podIP" {
		t.Errorf("LIVEKIT_NODE_IP = %v, want the pod's own address (status.podIP)", e)
	}
	if e, _ := envEntry(c, "LIVEKIT_KEYS"); dig(e, "value") != "$(LIVEKIT_API_KEY): $(LIVEKIT_API_SECRET)" {
		t.Errorf("LIVEKIT_KEYS = %v", e)
	}
	mounted := false
	for _, m := range c["volumeMounts"].([]any) {
		if dig(m, "mountPath") == "/etc/livekit/turn-tls" && dig(m, "readOnly") == true {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the TURN certificate is not mounted read-only at /etc/livekit/turn-tls")
	}
}

// 🔴 No direct media path: no Service names LiveKit's 7881 or 7882 or any
// UDP port, no pod uses the host's network or a hostPort, and the Services
// expose exactly the ports listed.
func TestKubernetesExposesNoDirectMedia(t *testing.T) {
	docs := render(t, "kubernetes")
	want := map[string]struct {
		typ   string
		ports []int
	}{
		"muster-meetings":  {"ClusterIP", []int{8080, 8081}},
		"livekit":          {"ClusterIP", []int{7880}},
		"livekit-nodeport": {"NodePort", []int{5349, 7880}},
	}
	svcs := ofKind(docs, "Service")
	if len(svcs) != len(want) {
		t.Errorf("%d Services rendered, want %d", len(svcs), len(want))
	}
	for _, s := range svcs {
		name, _ := dig(s, "metadata", "name").(string)
		var ports []int
		for _, p := range dig(s, "spec", "ports").([]any) {
			n, _ := toInt(dig(p, "port"))
			ports = append(ports, n)
			if proto, _ := dig(p, "protocol").(string); proto != "" && proto != "TCP" {
				t.Errorf("Service %s exposes %d/%s: no UDP in phase 2c", name, n, proto)
			}
			for _, k := range []string{"port", "targetPort"} {
				if v, _ := toInt(dig(p, k)); v == 7881 || v == 7882 {
					t.Errorf("Service %s exposes %d: LiveKit's direct media port", name, v)
				}
			}
		}
		sort.Ints(ports)
		w, ok := want[name]
		if !ok {
			t.Errorf("unexpected Service %s", name)
			continue
		}
		if typ := dig(s, "spec", "type"); typ != w.typ || !slices.Equal(ports, w.ports) {
			t.Errorf("Service %s: %v %v, want %s %v", name, typ, ports, w.typ, w.ports)
		}
	}
	for _, d := range ofKind(docs, "Deployment") {
		spec := dig(d, "spec", "template", "spec").(obj)
		if spec["hostNetwork"] == true {
			t.Errorf("Deployment %v uses hostNetwork", dig(d, "metadata", "name"))
		}
		for _, c := range spec["containers"].([]any) {
			for _, p := range dig(c, "ports").([]any) {
				if dig(p, "hostPort") != nil {
					t.Errorf("Deployment %v has a hostPort", dig(d, "metadata", "name"))
				}
			}
			if dig(c, "securityContext", "readOnlyRootFilesystem") != true || dig(c, "securityContext", "allowPrivilegeEscalation") != false {
				t.Errorf("Deployment %v: the container may write its root filesystem or escalate", dig(d, "metadata", "name"))
			}
		}
		if dig(spec, "securityContext", "runAsNonRoot") != true {
			t.Errorf("Deployment %v does not run as non-root", dig(d, "metadata", "name"))
		}
	}
}

// The plugin: one replica, Recreate, SQLite on the PVC at STATE_DIR, probes
// on the internal port's /healthz, http to LiveKit's ClusterIP Service.
func TestKubernetesPlugin(t *testing.T) {
	docs := render(t, "kubernetes")
	dep := find(t, docs, "Deployment", "muster-meetings")
	if n, _ := toInt(dig(dep, "spec", "replicas")); n != 1 || dig(dep, "spec", "strategy", "type") != "Recreate" {
		t.Errorf("plugin replicas %v, strategy %v; want 1, Recreate (one SQLite writer)", dig(dep, "spec", "replicas"), dig(dep, "spec", "strategy", "type"))
	}
	c := container(t, dep, "muster-meetings")
	internal := 0
	for _, p := range c["ports"].([]any) {
		if dig(p, "name") == "internal" {
			internal, _ = toInt(dig(p, "containerPort"))
		}
	}
	if internal != 8081 {
		t.Errorf("the internal port is %d, want 8081", internal)
	}
	for _, pr := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
		if dig(c, pr, "httpGet", "path") != "/healthz" || dig(c, pr, "httpGet", "port") != "internal" {
			t.Errorf("%s = %v, want GET /healthz on the internal port", pr, c[pr])
		}
	}
	if e, _ := envEntry(c, "STATE_DIR"); dig(e, "value") != "/state" {
		t.Errorf("STATE_DIR = %v, want /state", e)
	}
	stateOnPVC := false
	for _, m := range c["volumeMounts"].([]any) {
		if dig(m, "mountPath") == "/state" {
			for _, v := range dig(dep, "spec", "template", "spec", "volumes").([]any) {
				if dig(v, "name") == dig(m, "name") && dig(v, "persistentVolumeClaim", "claimName") == "muster-meetings-state" {
					stateOnPVC = true
				}
			}
		}
	}
	if !stateOnPVC {
		t.Error("/state is not the muster-meetings-state PVC")
	}
	find(t, docs, "PersistentVolumeClaim", "muster-meetings-state")
	if e, _ := envEntry(c, "MEETINGS_LIVEKIT_API_URL"); dig(e, "value") != "http://livekit:7880" {
		t.Errorf("MEETINGS_LIVEKIT_API_URL = %v", e)
	}
	if e, _ := envEntry(c, "MEETINGS_LIVEKIT_ICE"); dig(e, "value") != "relay" {
		t.Errorf("MEETINGS_LIVEKIT_ICE = %v, want relay (TURN over TLS is the only media path)", e)
	}
}

// 🔴 The params seam: every key a Deployment reads from the params
// ConfigMap exists in it, and every key in it is read (the set may neither
// grow nor shrink on one side alone).
func TestKubernetesParamsAreAllUsed(t *testing.T) {
	docs := render(t, "kubernetes")
	var cm obj
	for _, d := range ofKind(docs, "ConfigMap") {
		if n, _ := dig(d, "metadata", "name").(string); strings.HasPrefix(n, "muster-meetings-params-") {
			cm = d
		}
	}
	if cm == nil {
		t.Fatal("no muster-meetings-params ConfigMap")
	}
	var have, used []string
	for k := range cm["data"].(obj) {
		have = append(have, k)
	}
	for _, d := range ofKind(docs, "Deployment") {
		for _, c := range dig(d, "spec", "template", "spec", "containers").([]any) {
			for _, e := range dig(c, "env").([]any) {
				ref := dig(e, "valueFrom", "configMapKeyRef")
				if ref == nil {
					continue
				}
				if dig(ref, "name") != dig(cm, "metadata", "name") {
					t.Errorf("%v reads ConfigMap %v, want the params ConfigMap", dig(e, "name"), dig(ref, "name"))
				}
				if k, _ := dig(ref, "key").(string); !slices.Contains(used, k) {
					used = append(used, k)
				}
			}
		}
	}
	sort.Strings(have)
	sort.Strings(used)
	if !slices.Equal(have, used) {
		t.Errorf("params keys %v, read %v: they must match", have, used)
	}
}

// 🔴 NetworkPolicies: both pods default-deny both ways; the plugin's
// internal port admits LiveKit alone, its public port the ingress
// controller's namespace alone; LiveKit may call only the plugin's internal
// port (and DNS).
func TestKubernetesNetworkPolicies(t *testing.T) {
	docs := render(t, "kubernetes")
	for _, n := range []string{"muster-meetings", "livekit"} {
		np := find(t, docs, "NetworkPolicy", n)
		if dig(np, "spec", "podSelector", "matchLabels", "app.kubernetes.io/name") != n {
			t.Errorf("NetworkPolicy %s selects %v", n, dig(np, "spec", "podSelector"))
		}
		if !reflect.DeepEqual(dig(np, "spec", "policyTypes"), []any{"Ingress", "Egress"}) {
			t.Errorf("NetworkPolicy %s policyTypes = %v, want both", n, dig(np, "spec", "policyTypes"))
		}
	}
	// Which peers each plugin port admits.
	np := find(t, docs, "NetworkPolicy", "muster-meetings")
	admits := map[int][]string{}
	for _, r := range dig(np, "spec", "ingress").([]any) {
		froms, _ := dig(r, "from").([]any)
		var who []string
		for _, f := range froms {
			who = append(who, fmt.Sprint(f))
		}
		if len(froms) == 0 {
			who = []string{"ANYONE"}
		}
		for _, p := range dig(r, "ports").([]any) {
			n, _ := toInt(dig(p, "port"))
			admits[n] = append(admits[n], who...)
		}
	}
	if w := admits[8081]; len(w) != 1 || !strings.Contains(w[0], "app.kubernetes.io/name:livekit") || strings.Contains(w[0], "namespaceSelector") {
		t.Errorf("the plugin's :8081 admits %v, want LiveKit's pods alone", w)
	}
	if w := admits[8080]; len(w) != 1 || !strings.Contains(w[0], "namespaceSelector") {
		t.Errorf("the plugin's :8080 admits %v, want the ingress controller's namespace alone", w)
	}
	if len(admits) != 2 {
		t.Errorf("the plugin admits ports %v, want 8080 and 8081 only", admits)
	}
	lk := find(t, docs, "NetworkPolicy", "livekit")
	for _, r := range dig(lk, "spec", "egress").([]any) {
		for _, p := range dig(r, "ports").([]any) {
			n, _ := toInt(dig(p, "port"))
			if n != 8081 && n != 53 {
				t.Errorf("LiveKit may reach port %d, want only the plugin's 8081 and DNS", n)
			}
			if n == 8081 && !strings.Contains(fmt.Sprint(dig(r, "to")), "app.kubernetes.io/name:muster-meetings") {
				t.Errorf("LiveKit's :8081 egress goes to %v, want the plugin's pods", dig(r, "to"))
			}
		}
	}
}

// The example overlay renders, and every knob a tenant sets is set by it:
// the namespace, the image, the hostnames, the NodePorts, the StorageClass,
// a Secret's name and the NetworkPolicies' selectors.
func TestTheExampleOverlaySetsEveryKnob(t *testing.T) {
	docs := render(t, "example-overlay")
	for _, d := range docs {
		if dig(d, "metadata", "namespace") != "muster-tenant" {
			t.Errorf("%v %v has namespace %v", d["kind"], dig(d, "metadata", "name"), dig(d, "metadata", "namespace"))
		}
	}
	svc := find(t, docs, "Service", "livekit-nodeport")
	for i, want := range []int{30880, 30549} {
		if n, _ := toInt(dig(svc, "spec", "ports", i, "nodePort")); n != want {
			t.Errorf("nodePort %d = %v, want %d", i, dig(svc, "spec", "ports", i, "nodePort"), want)
		}
	}
	if dig(find(t, docs, "PersistentVolumeClaim", "muster-meetings-state"), "spec", "storageClassName") != "example-local" {
		t.Error("the overlay did not set the StorageClass")
	}
	lk := find(t, docs, "Deployment", "livekit")
	if dig(lk, "spec", "template", "spec", "volumes", 0, "secret", "secretName") != "example-turn-certificate" {
		t.Error("the overlay did not rename the TURN certificate's Secret")
	}
	if img, _ := dig(container(t, find(t, docs, "Deployment", "muster-meetings"), "muster-meetings"), "image").(string); img != "ghcr.io/zacxdev/muster-meetings:0.1.0" {
		t.Errorf("plugin image = %q", img)
	}
	for _, d := range ofKind(docs, "ConfigMap") {
		data := d["data"].(obj)
		if data["client-ip-header"] != "CF-Connecting-IP" || data["trusted-proxies"] != "192.0.2.0/24" || data["turn-domain"] != "meet-turn-muster.example.test" {
			t.Errorf("params = %v", data)
		}
	}
}

// composeEnv expands ${NAME}, ${NAME:-default} and ${NAME-default} as
// Compose does, from env.
func composeEnv(s string, env map[string]string) string {
	re := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		p := re.FindStringSubmatch(m)
		if v, ok := env[p[1]]; ok && v != "" {
			return v
		}
		return p[2]
	})
}

func readCompose(t *testing.T, env map[string]string) obj {
	t.Helper()
	raw, err := os.ReadFile("../compose.meetings.yml")
	if err != nil {
		t.Fatal(err)
	}
	var c obj
	if err := yaml.Unmarshal([]byte(composeEnv(string(raw), env)), &c); err != nil {
		t.Fatalf("compose.meetings.yml: %v", err)
	}
	return c
}

// composeFixture: values distinct from every default the file names.
var composeFixture = map[string]string{
	"MEETINGS_TURN_DOMAIN": "meet-turn-fixture.example.test",
	"LIVEKIT_API_KEY":      "APIcomposeFixture",
	"MEETINGS_LIVEKIT_IP":  "172.31.250.77",
}

// 🔴 LiveKit in Compose: the same invariants, LiveKit's address one value
// (node_ip, the TURN allow-list entry and the container's fixed address),
// and only signalling and TURN/TLS published.
func TestComposeLiveKitConfig(t *testing.T) {
	c := readCompose(t, composeFixture)
	lk := dig(c, "services", "livekit").(obj)
	if img, _ := lk["image"].(string); !liveKitImage.MatchString(img) {
		t.Errorf("compose livekit image = %q, want livekit-server v1.13.11 pinned by digest", img)
	}
	body, _ := dig(lk, "environment", "LIVEKIT_CONFIG").(string)
	checkLiveKitConfig(t, "compose", body, lkWant{domain: "meet-turn-fixture.example.test", nodeIP: "172.31.250.77", apiKey: "APIcomposeFixture"})
	if got := dig(lk, "networks", "meetings", "ipv4_address"); got != "172.31.250.77" {
		t.Errorf("compose livekit ipv4_address = %v, want rtc.node_ip's 172.31.250.77", got)
	}
	for svc, want := range map[string][]string{"livekit": {"5349", "7880"}, "muster-meetings": {"8080"}} {
		var got []string
		for _, p := range dig(c, "services", svc, "ports").([]any) {
			s := fmt.Sprint(p)
			if strings.Contains(s, "/udp") {
				t.Errorf("compose %s publishes %s: no UDP in phase 2c", svc, s)
			}
			parts := strings.Split(s, ":")
			if parts[0] != "127.0.0.1" {
				t.Errorf("compose %s publishes %s beyond 127.0.0.1", svc, s)
			}
			got = append(got, parts[len(parts)-1])
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("compose %s publishes container ports %v, want %v", svc, got, want)
		}
	}
	if pr := dig(c, "services", "livekit", "profiles"); !reflect.DeepEqual(pr, []any{"meetings"}) {
		t.Errorf("compose livekit profiles = %v", pr)
	}
	if pr := dig(c, "services", "muster-meetings", "profiles"); !reflect.DeepEqual(pr, []any{"meetings"}) {
		t.Errorf("compose muster-meetings profiles = %v", pr)
	}
}

// 🔴 One LiveKit config, two files: with the same per-deployment values,
// the Kubernetes and Compose configs are the same document.
func TestKubernetesAndComposeLiveKitConfigsAgree(t *testing.T) {
	docs := render(t, "kubernetes")
	e, _ := envEntry(container(t, find(t, docs, "Deployment", "livekit"), "livekit"), "LIVEKIT_CONFIG")
	kube := kubeVar.ReplaceAllStringFunc(e["value"].(string), func(m string) string {
		return map[string]string{"$(TURN_DOMAIN)": "meet-turn-fixture.example.test", "$(LIVEKIT_API_KEY)": "APIcomposeFixture", "$(LIVEKIT_NODE_IP)": "172.31.250.77"}[m]
	})
	compose, _ := dig(readCompose(t, composeFixture), "services", "livekit", "environment", "LIVEKIT_CONFIG").(string)
	var a, b obj
	if err := yaml.Unmarshal([]byte(kube), &a); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(compose), &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the LiveKit configs differ:\nkubernetes: %v\ncompose:    %v", a, b)
	}
}

// 🔴 A public repository: the deployment files name no real host or
// address. Every URL's host is example.test or a cluster-internal name, and
// every IPv4 literal is a documentation, loopback or Compose-default one.
func TestTheDeploymentFilesNameNoRealHost(t *testing.T) {
	files := []string{"../compose.meetings.yml", "../Dockerfile", "../.env.example"}
	if err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			files = append(files, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	urlHost := regexp.MustCompile(`(?:https?|wss?)://([A-Za-z0-9.-]+)`)
	ipv4 := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	allowedNets := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("172.31.250.0/24"), netip.MustParsePrefix("10.0.0.0/8"),
	}
	seen := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range urlHost.FindAllStringSubmatch(string(raw), -1) {
			seen++
			h := strings.ToLower(m[1])
			ok := h == "example.test" || strings.HasSuffix(h, ".example.test") || !strings.Contains(h, ".") ||
				strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local") ||
				h == "github.com" || h == "127.0.0.1"
			if !ok {
				t.Errorf("%s names the host %q: use example.test", f, h)
			}
		}
		for _, m := range ipv4.FindAllString(string(raw), -1) {
			a, err := netip.ParseAddr(m)
			if err != nil {
				continue
			}
			if !slices.ContainsFunc(allowedNets, func(p netip.Prefix) bool { return p.Contains(a) }) {
				t.Errorf("%s names the address %s: use a documentation address (RFC 5737)", f, m)
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d URLs found in %d files: the scan is not reading them", seen, len(files))
	}
}
