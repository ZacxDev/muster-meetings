// Command e2e-harness runs the plugin against the fake muster for the
// browser test in e2e/: the plugin on one origin, and a muster-like framing
// parent on another, whose /apps/<id> page frames the plugin's /enter exactly
// as muster's does. It prints one JSON line,
//
//	{"plugin": "http://127.0.0.1:<port>", "parent": "http://localhost:<port>", "plugin_id": 12}
//
// and serves until its standard input closes or it is signalled. The two
// origins are different sites, as meet.<muster host> and muster are.
//
// The parent's routes, for the test only:
//
//	GET /mint                  a fresh hand-off code, as the person's browser mints one
//	GET /redeems               how many redeems the fake muster has answered
//	GET /apps/<id>[?code=…]    muster's page: frames /enter with that code, or a fresh one
//	GET /livekit-joins         every token a browser presented to the fake LiveKit, verified
//
// With -livekit=real it starts a real livekit-server instead (the binary in
// MEETINGS_LIVEKIT_SERVER, or on PATH) and says "real_livekit": true; with
// no binary it says false and the spec that needs one decides.
//
// LiveKit is a fake (internal/livekittest): rooms and participants over its
// server API, and a signalling endpoint that records, verified, the token a
// browser connects with and refuses the connection (it carries no media).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

var (
	app = mustertest.App{Token: "mpl_e2e_server_6a0f2c", PluginID: 12, PluginName: "meet-e2e", Capabilities: []string{"app"}}
	ada = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
)

var lkMode = flag.String("livekit", "fake", "fake, or real (a livekit-server binary)")

func main() {
	flag.Parse()
	logger := log.New(os.Stderr, "e2e-harness: ", 0)
	if err := run(logger); err != nil {
		logger.Fatal(err)
	}
}

func listen(host string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, "http://" + host + ":" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port), nil
}

func run(logger *log.Logger) error {
	fake := mustertest.New()
	defer fake.Close()
	fake.Install(app)
	fake.AddPerson(ada)

	pluginLn, pluginURL, err := listen("127.0.0.1")
	if err != nil {
		return err
	}
	parentLn, parentOrigin, err := listen("localhost")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "meetings-e2e-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(filepath.Join(dir, "meetings.db"), []byte("e2e harness link key 0123456789a"))
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := muster.New(fake.URL, app.Token)
	if err != nil {
		return err
	}
	a, err := owners.Boot(context.Background(), c, st, logger, time.Now)
	if err != nil {
		return err
	}
	lk := livekittest.New()
	defer lk.Close()
	lkAPI, lkWS, lkKeys := lk.URL, lk.WSURL(), livekittest.Keys
	internalLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	real := false
	if *lkMode == "real" {
		if api, stop, err := startLiveKit(logger, "http://"+internalLn.Addr().String()); err != nil {
			logger.Printf("no real LiveKit: %v", err)
		} else {
			defer stop()
			real, lkAPI, lkWS, lkKeys = true, api, "ws"+strings.TrimPrefix(api, "http"), realKeys
		}
	}
	lkc, err := livekit.NewClient(lkAPI, lkKeys)
	if err != nil {
		return err
	}
	srv := server.New(server.Config{PublicURL: pluginURL, MusterOrigin: parentOrigin, Log: logger,
		LiveKit: lkc, LiveKitURL: lkWS, ICE: "all"}, st, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	plugin := &http.Server{Handler: srv}
	go plugin.Serve(pluginLn)
	internal := &http.Server{Handler: srv.Internal()}
	go internal.Serve(internalLn)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /mint", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fake.MintApp(app.PluginID, ada.ID, ""))
	})
	mux.HandleFunc("GET /redeems", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strconv.Itoa(fake.CountCalls("POST", "/plugin/v1/handoffs/redeem")))
	})
	mux.HandleFunc("GET /livekit-joins", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lk.RTCs())
	})
	mux.HandleFunc("GET /apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			// ?ref= as muster passes it from a knock notification's tap.
			code = fake.MintApp(app.PluginID, ada.ID, r.URL.Query().Get("ref"))
		}
		src := pluginURL + "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(app.PluginID, 10)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// muster's frame attributes for an app tab (internal/ui/app_page.go:
		// PluginChipSandbox, referrerpolicy no-referrer).
		// The frame fills the page, as muster's /apps/{id} draws it below a
		// slim header (design: "one frame fills the rest of the viewport").
		fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1"><title>muster</title>
<body style="margin:0"><header style="height:44px;font:16px system-ui;line-height:44px;padding:0 8px">muster</header>
<iframe id="app" src="%s" sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals" allow="camera %s; microphone %s; display-capture %s; fullscreen %s; autoplay %s; clipboard-write %s" referrerpolicy="no-referrer" style="display:block;border:0;width:100%%;height:calc(100dvh - 44px)"></iframe></body>`,
			html.EscapeString(src), pluginURL, pluginURL, pluginURL, pluginURL, pluginURL, pluginURL)
	})
	parent := &http.Server{Handler: mux}
	go parent.Serve(parentLn)

	b, _ := json.Marshal(map[string]any{"plugin": pluginURL, "parent": parentOrigin, "plugin_id": app.PluginID, "real_livekit": real})
	fmt.Println(string(b))

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sig:
	}
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	plugin.Shutdown(sctx)
	parent.Shutdown(sctx)
	internal.Shutdown(sctx)
	return nil
}

var realKeys = livekit.Keys{Key: "APIe2eharness", Secret: "an-e2e-harness-livekit-secret-0123456789"}

// startLiveKit runs livekit-server on free loopback ports, its webhook
// pointed at the plugin's internal listener, and waits until it answers.
func startLiveKit(logger *log.Logger, internalURL string) (string, func(), error) {
	bin := os.Getenv("MEETINGS_LIVEKIT_SERVER")
	if bin == "" {
		bin, _ = exec.LookPath("livekit-server")
	}
	if bin == "" {
		return "", nil, errors.New("livekit-server not found (MEETINGS_LIVEKIT_SERVER or PATH)")
	}
	free := func(network string) int {
		if network == "udp" {
			c, _ := net.ListenPacket("udp", "127.0.0.1:0")
			defer c.Close()
			return c.LocalAddr().(*net.UDPAddr).Port
		}
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	port := free("tcp")
	dir, err := os.MkdirTemp("", "meetings-e2e-livekit-")
	if err != nil {
		return "", nil, err
	}
	// The media address LiveKit advertises: the host's own address, not
	// 127.0.0.1. Chromium does not reach a loopback ICE candidate on every
	// host (measured on the CI runner: signalling connected, then "could not
	// establish pc connection"), while LiveKit's CLI (pion) does.
	nodeIP := hostIP()
	cfg := fmt.Sprintf("port: %d\nbind_addresses: [\"127.0.0.1\"]\nrtc:\n  tcp_port: %d\n  udp_port: %d\n  use_external_ip: false\n  node_ip: %s\nkeys:\n  %s: %s\nwebhook:\n  api_key: %s\n  urls: [\"%s/livekit/webhook\"]\n",
		port, free("tcp"), free("udp"), nodeIP, realKeys.Key, realKeys.Secret, realKeys.Key, internalURL)
	path := filepath.Join(dir, "livekit.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		return "", nil, err
	}
	cmd := exec.Command(bin, "--config", path)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() { cmd.Process.Kill(); cmd.Wait(); os.RemoveAll(dir) }
	api := "http://127.0.0.1:" + strconv.Itoa(port)
	for end := time.Now().Add(20 * time.Second); ; {
		if resp, err := http.Get(api); err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(end) {
			stop()
			return "", nil, errors.New("livekit-server did not answer in 20 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	logger.Printf("real livekit-server on %s (media on %s)", api, nodeIP)
	return api, stop, nil
}

// hostIP is the first IPv4 address of an interface that is up and not
// loopback, or 127.0.0.1 when there is none.
func hostIP() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
