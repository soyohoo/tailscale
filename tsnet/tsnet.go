// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package tsnet provides Tailscale as a library.
//
// It is an experimental work in progress.
package tsnet

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/control/controlclient"
	"tailscale.com/envknob"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/localapi"
	"tailscale.com/ipn/store"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/logpolicy"
	"tailscale.com/logtail"
	"tailscale.com/logtail/filch"
	"tailscale.com/net/memnet"
	"tailscale.com/net/proxymux"
	"tailscale.com/net/socks5"
	"tailscale.com/net/tsdial"
	"tailscale.com/smallzstd"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
	"tailscale.com/util/mak"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/monitor"
	"tailscale.com/wgengine/netstack"
)

// Server is an embedded Tailscale server.
//
// Its exported fields may be changed until the first call to Listen.
type Server struct {
	// Dir specifies the name of the directory to use for
	// state. If empty, a directory is selected automatically
	// under os.UserConfigDir (https://golang.org/pkg/os/#UserConfigDir).
	// based on the name of the binary.
	Dir string

	// Store specifies the state store to use.
	//
	// If nil, a new FileStore is initialized at `Dir/tailscaled.state`.
	// See tailscale.com/ipn/store for supported stores.
	//
	// Logs will automatically be uploaded to uploaded to log.tailscale.io,
	// where the configuration file for logging will be saved at
	// `Dir/tailscaled.log.conf`.
	Store ipn.StateStore

	// Hostname is the hostname to present to the control server.
	// If empty, the binary name is used.
	Hostname string

	// Logf, if non-nil, specifies the logger to use. By default,
	// log.Printf is used.
	Logf logger.Logf

	// Ephemeral, if true, specifies that the instance should register
	// as an Ephemeral node (https://tailscale.com/kb/1111/ephemeral-nodes/).
	Ephemeral bool

	// AuthKey, if non-empty, is the auth key to create the node
	// and will be preferred over the TS_AUTHKEY environment
	// variable. If the node is already created (from state
	// previously stored in in Store), then this field is not
	// used.
	AuthKey string

	// ControlURL optionally specifies the coordination server URL.
	// If empty, the Tailscale default is used.
	ControlURL string

	initOnce         sync.Once
	initErr          error
	lb               *ipnlocal.LocalBackend
	netstack         *netstack.Impl
	linkMon          *monitor.Mon
	rootPath         string // the state directory
	hostname         string
	shutdownCtx      context.Context
	shutdownCancel   context.CancelFunc
	proxyCred        string                 // SOCKS5 proxy auth for loopbackListener
	localAPICred     string                 // basic auth password for loopbackListener
	loopbackListener net.Listener           // optional loopback for localapi and proxies
	localAPIListener net.Listener           // in-memory, used by localClient
	localClient      *tailscale.LocalClient // in-memory
	logbuffer        *filch.Filch
	logtail          *logtail.Logger
	logid            string

	mu        sync.Mutex
	listeners map[listenKey]*listener
	dialer    *tsdial.Dialer
}

// Dial connects to the address on the tailnet.
// It will start the server if it has not been started yet.
func (s *Server) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s.dialer.UserDial(ctx, network, address)
}

// HTTPClient returns an HTTP client that is configured to connect over Tailscale.
//
// This is useful if you need to have your tsnet services connect to other devices on
// your tailnet.
func (s *Server) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: s.Dial,
		},
	}
}

// LocalClient returns a LocalClient that speaks to s.
//
// It will start the server if it has not been started yet. If the server's
// already been started successfully, it doesn't return an error.
func (s *Server) LocalClient() (*tailscale.LocalClient, error) {
	if err := s.Start(); err != nil {
		return nil, err
	}
	return s.localClient, nil
}

// Loopback starts a routing server on a loopback address.
//
// The server has multiple functions.
//
// It can be used as a SOCKS5 proxy onto the tailnet.
// Authentication is required with the username "tsnet" and
// the value of proxyCred used as the password.
//
// The HTTP server also serves out the "LocalAPI" on /localapi.
// As the LocalAPI is powerful, access to endpoints requires BOTH passing a
// "Sec-Tailscale: localapi" HTTP header and passing localAPICred as basic auth.
//
// If you only need to use the LocalAPI from Go, then prefer LocalClient
// as it does not require communication via TCP.
func (s *Server) Loopback() (addr string, proxyCred, localAPICred string, err error) {
	if err := s.Start(); err != nil {
		return "", "", "", err
	}

	if s.loopbackListener == nil {
		var proxyCred [16]byte
		if _, err := crand.Read(proxyCred[:]); err != nil {
			return "", "", "", err
		}
		s.proxyCred = hex.EncodeToString(proxyCred[:])

		var cred [16]byte
		if _, err := crand.Read(cred[:]); err != nil {
			return "", "", "", err
		}
		s.localAPICred = hex.EncodeToString(cred[:])

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", "", "", err
		}
		s.loopbackListener = ln

		socksLn, httpLn := proxymux.SplitSOCKSAndHTTP(ln)

		// TODO: add HTTP proxy support. Probably requires factoring
		// out the CONNECT code from tailscaled/proxy.go that uses
		// httputil.ReverseProxy and adding auth support.
		go func() {
			lah := localapi.NewHandler(s.lb, s.logf, s.logid)
			lah.PermitWrite = true
			lah.PermitRead = true
			lah.RequiredPassword = s.localAPICred
			h := &localSecHandler{h: lah, cred: s.localAPICred}

			if err := http.Serve(httpLn, h); err != nil {
				s.logf("localapi tcp serve error: %v", err)
			}
		}()

		s5s := &socks5.Server{
			Logf:     logger.WithPrefix(s.logf, "socks5: "),
			Dialer:   s.dialer.UserDial,
			Username: "tsnet",
			Password: s.proxyCred,
		}
		go func() {
			s.logf("SOCKS5 server exited: %v", s5s.Serve(socksLn))
		}()
	}

	lbAddr := s.loopbackListener.Addr()
	if lbAddr == nil {
		// https://github.com/tailscale/tailscale/issues/7488
		panic("loopbackListener has no Addr")
	}
	return lbAddr.String(), s.proxyCred, s.localAPICred, nil
}

type localSecHandler struct {
	h    http.Handler
	cred string
}

func (h *localSecHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Tailscale") != "localapi" {
		w.WriteHeader(403)
		io.WriteString(w, "missing 'Sec-Tailscale: localapi' header")
		return
	}
	h.h.ServeHTTP(w, r)
}

// Start connects the server to the tailnet.
// Optional: any calls to Dial/Listen will also call Start.
func (s *Server) Start() error {
	hostinfo.SetPackage("tsnet")
	s.initOnce.Do(s.doInit)
	return s.initErr
}

// Up connects the server to the tailnet and waits until it is running.
// On success it returns the current status, including a Tailscale IP address.
func (s *Server) Up(ctx context.Context) (*ipnstate.Status, error) {
	lc, err := s.LocalClient() // calls Start
	if err != nil {
		return nil, fmt.Errorf("tsnet.Up: %w", err)
	}

	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyNoPrivateKeys)
	if err != nil {
		return nil, fmt.Errorf("tsnet.Up: %w", err)
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			return nil, fmt.Errorf("tsnet.Up: %w", err)
		}
		if n.ErrMessage != nil {
			return nil, fmt.Errorf("tsnet.Up: backend: %s", *n.ErrMessage)
		}
		if s := n.State; s != nil {
			if *s == ipn.Running {
				status, err := lc.Status(ctx)
				if err != nil {
					return nil, fmt.Errorf("tsnet.Up: %w", err)
				}
				if len(status.TailscaleIPs) == 0 {
					return nil, errors.New("tsnet.Up: running, but no ip")
				}
				return status, nil
			}
			// TODO: in the future, return an error on ipn.NeedsLogin
			// and ipn.NeedsMachineAuth to improve the UX of trying
			// out the tsnet package.
			//
			// Unfortunately today, even when using an AuthKey we
			// briefly see these states. It would be nice to fix.
		}
	}
}

// Close stops the server.
//
// It must not be called before or concurrently with Start.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Perform a best-effort final flush.
		if s.logtail != nil {
			s.logtail.Shutdown(ctx)
		}
		if s.logbuffer != nil {
			s.logbuffer.Close()
		}
	}()

	if _, isMemStore := s.Store.(*mem.Store); isMemStore && s.Ephemeral && s.lb != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Perform a best-effort logout.
			s.lb.LogoutSync(ctx)
		}()
	}

	if s.netstack != nil {
		s.netstack.Close()
		s.netstack = nil
	}
	if s.shutdownCancel != nil {
		s.shutdownCancel()
	}
	if s.lb != nil {
		s.lb.Shutdown()
	}
	if s.linkMon != nil {
		s.linkMon.Close()
	}
	if s.dialer != nil {
		s.dialer.Close()
	}
	if s.localAPIListener != nil {
		s.localAPIListener.Close()
	}
	if s.loopbackListener != nil {
		s.loopbackListener.Close()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ln := range s.listeners {
		ln.Close()
	}
	s.listeners = nil

	wg.Wait()
	return nil
}

func (s *Server) doInit() {
	s.shutdownCtx, s.shutdownCancel = context.WithCancel(context.Background())
	if err := s.start(); err != nil {
		s.initErr = fmt.Errorf("tsnet: %w", err)
	}
}

// TailscaleIPs returns IPv4 and IPv6 addresses for this node. If the node
// has not yet joined a tailnet or is otherwise unaware of its own IP addresses,
// the returned ip4, ip6 will be !netip.IsValid().
func (s *Server) TailscaleIPs() (ip4, ip6 netip.Addr) {
	nm := s.lb.NetMap()
	for _, addr := range nm.Addresses {
		ip := addr.Addr()
		if ip.Is6() {
			ip6 = ip
		}
		if ip.Is4() {
			ip4 = ip
		}
	}

	return ip4, ip6
}

func (s *Server) getAuthKey() string {
	if v := s.AuthKey; v != "" {
		return v
	}
	return os.Getenv("TS_AUTHKEY")
}

func (s *Server) start() (reterr error) {
	var closePool closeOnErrorPool
	defer closePool.closeAllIfError(&reterr)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	prog := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")

	s.hostname = s.Hostname
	if s.hostname == "" {
		s.hostname = prog
	}

	s.rootPath = s.Dir
	if s.Store != nil {
		_, isMemStore := s.Store.(*mem.Store)
		if isMemStore && !s.Ephemeral {
			return fmt.Errorf("in-memory store is only supported for Ephemeral nodes")
		}
	}

	logf := s.logf

	if s.rootPath == "" {
		confDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		s.rootPath, err = getTSNetDir(logf, confDir, prog)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(s.rootPath, 0700); err != nil {
			return err
		}
	}
	if fi, err := os.Stat(s.rootPath); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%v is not a directory", s.rootPath)
	}

	cfgPath := filepath.Join(s.rootPath, "tailscaled.log.conf")

	lpc, err := logpolicy.ConfigFromFile(cfgPath)
	switch {
	case os.IsNotExist(err):
		lpc = logpolicy.NewConfig(logtail.CollectionNode)
		if err := lpc.Save(cfgPath); err != nil {
			return fmt.Errorf("logpolicy.Config.Save for %v: %w", cfgPath, err)
		}
	case err != nil:
		return fmt.Errorf("logpolicy.LoadConfig for %v: %w", cfgPath, err)
	}
	if err := lpc.Validate(logtail.CollectionNode); err != nil {
		return fmt.Errorf("logpolicy.Config.Validate for %v: %w", cfgPath, err)
	}
	s.logid = lpc.PublicID.String()

	s.logbuffer, err = filch.New(filepath.Join(s.rootPath, "tailscaled"), filch.Options{ReplaceStderr: false})
	if err != nil {
		return fmt.Errorf("error creating filch: %w", err)
	}
	closePool.add(s.logbuffer)
	c := logtail.Config{
		Collection: lpc.Collection,
		PrivateID:  lpc.PrivateID,
		Stderr:     io.Discard, // log everything to Buffer
		Buffer:     s.logbuffer,
		NewZstdEncoder: func() logtail.Encoder {
			w, err := smallzstd.NewEncoder(nil)
			if err != nil {
				panic(err)
			}
			return w
		},
		HTTPC: &http.Client{Transport: logpolicy.NewLogtailTransport(logtail.DefaultHost)},
	}
	s.logtail = logtail.NewLogger(c, logf)
	closePool.addFunc(func() { s.logtail.Shutdown(context.Background()) })

	s.linkMon, err = monitor.New(logf)
	if err != nil {
		return err
	}
	closePool.add(s.linkMon)

	s.dialer = &tsdial.Dialer{Logf: logf} // mutated below (before used)
	eng, err := wgengine.NewUserspaceEngine(logf, wgengine.Config{
		ListenPort:  0,
		LinkMonitor: s.linkMon,
		Dialer:      s.dialer,
	})
	if err != nil {
		return err
	}
	closePool.add(s.dialer)

	tunDev, magicConn, dns, ok := eng.(wgengine.InternalsGetter).GetInternals()
	if !ok {
		return fmt.Errorf("%T is not a wgengine.InternalsGetter", eng)
	}

	ns, err := netstack.Create(logf, tunDev, eng, magicConn, s.dialer, dns)
	if err != nil {
		return fmt.Errorf("netstack.Create: %w", err)
	}
	ns.ProcessLocalIPs = true
	ns.GetTCPHandlerForFlow = s.getTCPHandlerForFlow
	ns.GetUDPHandlerForFlow = s.getUDPHandlerForFlow
	s.netstack = ns
	s.dialer.UseNetstackForIP = func(ip netip.Addr) bool {
		_, ok := eng.PeerForIP(ip)
		return ok
	}
	s.dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		return ns.DialContextTCP(ctx, dst)
	}

	if s.Store == nil {
		stateFile := filepath.Join(s.rootPath, "tailscaled.state")
		logf("tsnet running state path %s", stateFile)
		s.Store, err = store.New(logf, stateFile)
		if err != nil {
			return err
		}
	}

	loginFlags := controlclient.LoginDefault
	if s.Ephemeral {
		loginFlags = controlclient.LoginEphemeral
	}
	lb, err := ipnlocal.NewLocalBackend(logf, s.logid, s.Store, s.dialer, eng, loginFlags)
	if err != nil {
		return fmt.Errorf("NewLocalBackend: %v", err)
	}
	lb.SetVarRoot(s.rootPath)
	logf("tsnet starting with hostname %q, varRoot %q", s.hostname, s.rootPath)
	s.lb = lb
	if err := ns.Start(lb); err != nil {
		return fmt.Errorf("failed to start netstack: %w", err)
	}
	closePool.addFunc(func() { s.lb.Shutdown() })
	lb.SetDecompressor(func() (controlclient.Decompressor, error) {
		return smallzstd.NewDecoder(nil)
	})
	prefs := ipn.NewPrefs()
	prefs.Hostname = s.hostname
	prefs.WantRunning = true
	prefs.ControlURL = s.ControlURL
	authKey := s.getAuthKey()
	err = lb.Start(ipn.Options{
		UpdatePrefs: prefs,
		AuthKey:     authKey,
	})
	if err != nil {
		return fmt.Errorf("starting backend: %w", err)
	}
	st := lb.State()
	if st == ipn.NeedsLogin || envknob.Bool("TSNET_FORCE_LOGIN") {
		logf("LocalBackend state is %v; running StartLoginInteractive...", st)
		s.lb.StartLoginInteractive()
	} else if authKey != "" {
		logf("Authkey is set; but state is %v. Ignoring authkey. Re-run with TSNET_FORCE_LOGIN=1 to force use of authkey.", st)
	}
	go s.printAuthURLLoop()

	// Run the localapi handler, to allow fetching LetsEncrypt certs.
	lah := localapi.NewHandler(lb, logf, s.logid)
	lah.PermitWrite = true
	lah.PermitRead = true

	// Create an in-process listener.
	// nettest.Listen provides a in-memory pipe based implementation for net.Conn.
	lal := memnet.Listen("local-tailscaled.sock:80")
	s.localAPIListener = lal
	s.localClient = &tailscale.LocalClient{Dial: lal.Dial}
	go func() {
		if err := http.Serve(lal, lah); err != nil {
			logf("localapi serve error: %v", err)
		}
	}()
	closePool.add(s.localAPIListener)
	return nil
}

type closeOnErrorPool []func()

func (p *closeOnErrorPool) add(c io.Closer)   { *p = append(*p, func() { c.Close() }) }
func (p *closeOnErrorPool) addFunc(fn func()) { *p = append(*p, fn) }
func (p closeOnErrorPool) closeAllIfError(errp *error) {
	if *errp != nil {
		for _, closeFn := range p {
			closeFn()
		}
	}
}

func (s *Server) logf(format string, a ...interface{}) {
	if s.logtail != nil {
		s.logtail.Logf(format, a...)
	}
	if s.Logf != nil {
		s.Logf(format, a...)
		return
	}
	log.Printf(format, a...)
}

// printAuthURLLoop loops once every few seconds while the server is still running and
// is in NeedsLogin state, printing out the auth URL.
func (s *Server) printAuthURLLoop() {
	for {
		if s.shutdownCtx.Err() != nil {
			return
		}
		if st := s.lb.State(); st != ipn.NeedsLogin {
			s.logf("printAuthURLLoop: state is %v; stopping", st)
			return
		}
		st := s.lb.StatusWithoutPeers()
		if st.AuthURL != "" {
			s.logf("To start this tsnet server, restart with TS_AUTHKEY set, or go to: %s", st.AuthURL)
		}
		select {
		case <-time.After(5 * time.Second):
		case <-s.shutdownCtx.Done():
			return
		}
	}
}

// networkForFamily returns one of "tcp4", "tcp6", "udp4", or "udp6".
//
// netBase is "tcp" or "udp" (without any '4' or '6' suffix).
func networkForFamily(netBase string, is6 bool) string {
	switch netBase {
	case "tcp":
		if is6 {
			return "tcp6"
		}
		return "tcp4"
	case "udp":
		if is6 {
			return "udp6"
		}
		return "udp4"
	}
	panic("unexpected")
}

// listenerForDstAddr returns a listener for the provided network and
// destination IP/port. It matches from most specific to least specific.
// For example:
//
//   - ("tcp4", IP, port)
//   - ("tcp", IP, port)
//   - ("tcp4", "", port)
//   - ("tcp", "", port)
//
// The netBase is "tcp" or "udp" (without any '4' or '6' suffix).
func (s *Server) listenerForDstAddr(netBase string, dst netip.AddrPort) (_ *listener, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range [2]netip.Addr{0: dst.Addr()} {
		for _, net := range [2]string{
			networkForFamily(netBase, dst.Addr().Is6()),
			netBase,
		} {
			if ln, ok := s.listeners[listenKey{net, a, dst.Port()}]; ok {
				return ln, true
			}
		}
	}
	return nil, false
}

func (s *Server) getTCPHandlerForFlow(src, dst netip.AddrPort) (handler func(net.Conn), intercept bool) {
	ln, ok := s.listenerForDstAddr("tcp", dst)
	if !ok {
		return nil, true // don't handle, don't forward to localhost
	}
	return ln.handle, true
}

func (s *Server) getUDPHandlerForFlow(src, dst netip.AddrPort) (handler func(nettype.ConnPacketConn), intercept bool) {
	ln, ok := s.listenerForDstAddr("udp", dst)
	if !ok {
		return nil, true // don't handle, don't forward to localhost
	}
	return func(c nettype.ConnPacketConn) { ln.handle(c) }, true
}

// getTSNetDir usually just returns filepath.Join(confDir, "tsnet-"+prog)
// with no error.
//
// One special case is that it renames old "tslib-" directories to
// "tsnet-", and that rename might return an error.
//
// TODO(bradfitz): remove this maybe 6 months after 2022-03-17,
// once people (notably Tailscale corp services) have updated.
func getTSNetDir(logf logger.Logf, confDir, prog string) (string, error) {
	oldPath := filepath.Join(confDir, "tslib-"+prog)
	newPath := filepath.Join(confDir, "tsnet-"+prog)

	fi, err := os.Lstat(oldPath)
	if os.IsNotExist(err) {
		// Common path.
		return newPath, nil
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("expected old tslib path %q to be a directory; got %v", oldPath, fi.Mode())
	}

	// At this point, oldPath exists and is a directory. But does
	// the new path exist?

	fi, err = os.Lstat(newPath)
	if err == nil && fi.IsDir() {
		// New path already exists somehow. Ignore the old one and
		// don't try to migrate it.
		return newPath, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return "", err
	}
	logf("renamed old tsnet state storage directory %q to %q", oldPath, newPath)
	return newPath, nil
}

// APIClient returns a tailscale.Client that can be used to make authenticated
// requests to the Tailscale control server.
// It requires the user to set tailscale.I_Acknowledge_This_API_Is_Unstable.
func (s *Server) APIClient() (*tailscale.Client, error) {
	if !tailscale.I_Acknowledge_This_API_Is_Unstable {
		return nil, errors.New("use of Client without setting I_Acknowledge_This_API_Is_Unstable")
	}
	if err := s.Start(); err != nil {
		return nil, err
	}

	c := tailscale.NewClient("-", nil)
	c.HTTPClient = &http.Client{Transport: s.lb.KeyProvingNoiseRoundTripper()}
	return c, nil
}

// Listen announces only on the Tailscale network.
// It will start the server if it has not been started yet.
func (s *Server) Listen(network, addr string) (net.Listener, error) {
	switch network {
	case "", "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return nil, errors.New("unsupported network type")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("tsnet: %w", err)
	}
	port, err := net.LookupPort(network, portStr)
	if err != nil || port < 0 || port > math.MaxUint16 {
		// LookupPort returns an error on out of range values so the bounds
		// checks on port should be unnecessary, but harmless. If they do
		// match, worst case this error message says "invalid port: <nil>".
		return nil, fmt.Errorf("invalid port: %w", err)
	}
	var bindHostOrZero netip.Addr
	if host != "" {
		bindHostOrZero, err = netip.ParseAddr(host)
		if err != nil {
			return nil, fmt.Errorf("invalid Listen addr %q; host part must be empty or IP literal", host)
		}
		if strings.HasSuffix(network, "4") && !bindHostOrZero.Is4() {
			return nil, fmt.Errorf("invalid non-IPv4 addr %v for network %q", host, network)
		}
		if strings.HasSuffix(network, "6") && !bindHostOrZero.Is6() {
			return nil, fmt.Errorf("invalid non-IPv6 addr %v for network %q", host, network)
		}
	}

	if err := s.Start(); err != nil {
		return nil, err
	}

	key := listenKey{network, bindHostOrZero, uint16(port)}
	ln := &listener{
		s:    s,
		key:  key,
		addr: addr,

		conn: make(chan net.Conn),
	}
	s.mu.Lock()
	if _, ok := s.listeners[key]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("tsnet: listener already open for %s, %s", network, addr)
	}
	mak.Set(&s.listeners, key, ln)
	s.mu.Unlock()
	return ln, nil
}

type listenKey struct {
	network string
	host    netip.Addr // or zero value for unspecified
	port    uint16
}

type listener struct {
	s    *Server
	key  listenKey
	addr string
	conn chan net.Conn
}

func (ln *listener) Accept() (net.Conn, error) {
	c, ok := <-ln.conn
	if !ok {
		return nil, fmt.Errorf("tsnet: %w", net.ErrClosed)
	}
	return c, nil
}

func (ln *listener) Addr() net.Addr { return addr{ln} }
func (ln *listener) Close() error {
	ln.s.mu.Lock()
	defer ln.s.mu.Unlock()
	if v, ok := ln.s.listeners[ln.key]; ok && v == ln {
		delete(ln.s.listeners, ln.key)
		close(ln.conn)
	}
	return nil
}

func (ln *listener) handle(c net.Conn) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case ln.conn <- c:
	case <-t.C:
		// TODO(bradfitz): this isn't ideal. Think about how
		// we how we want to do pushback.
		c.Close()
	}
}

// Server returns the tsnet Server associated with the listener.
func (ln *listener) Server() *Server { return ln.s }

type addr struct{ ln *listener }

func (a addr) Network() string { return a.ln.key.network }
func (a addr) String() string  { return a.ln.addr }
