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
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

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

func main() {
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
	plugin := &http.Server{Handler: server.New(server.Config{PublicURL: pluginURL, MusterOrigin: parentOrigin, Log: logger}, st, a)}
	go plugin.Serve(pluginLn)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /mint", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fake.MintApp(app.PluginID, ada.ID, ""))
	})
	mux.HandleFunc("GET /redeems", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strconv.Itoa(fake.CountCalls("POST", "/plugin/v1/handoffs/redeem")))
	})
	mux.HandleFunc("GET /apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			code = fake.MintApp(app.PluginID, ada.ID, "")
		}
		src := pluginURL + "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(app.PluginID, 10)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// muster's frame attributes for an app tab (internal/ui/app_page.go:
		// PluginChipSandbox, referrerpolicy no-referrer).
		fmt.Fprintf(w, `<!doctype html><title>muster</title><h1>muster</h1>
<iframe id="app" src="%s" sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals" allow="camera %s; microphone %s; display-capture %s" referrerpolicy="no-referrer" style="width:900px;height:600px"></iframe>`,
			html.EscapeString(src), pluginURL, pluginURL, pluginURL)
	})
	parent := &http.Server{Handler: mux}
	go parent.Serve(parentLn)

	b, _ := json.Marshal(map[string]any{"plugin": pluginURL, "parent": parentOrigin, "plugin_id": app.PluginID})
	fmt.Println(string(b))

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sig:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	plugin.Shutdown(ctx)
	parent.Shutdown(ctx)
	return nil
}
