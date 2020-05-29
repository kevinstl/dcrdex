// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

// Package rpcserver provides a JSON RPC to communicate with the client core.
package rpcserver

import (
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/certgen"
	"github.com/decred/slog"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

const (
	// rpcTimeoutSeconds is the number of seconds a connection to the
	// RPC server is allowed to stay open without authenticating before it
	// is closed.
	rpcTimeoutSeconds = 10

	// RPC version
	rpcSemverMajor = 0
	rpcSemverMinor = 0
	rpcSemverPatch = 0
)

var (
	// Check that core.Core satifies ClientCore.
	_   ClientCore = (*core.Core)(nil)
	log slog.Logger
	// errUnknownCmd is wrapped when the command is not know.
	errUnknownCmd = errors.New("unknown command")
)

// ClientCore is satisfied by core.Core.
type ClientCore interface {
	Balance(assetID uint32) (baseUnits uint64, err error)
	Book(dex string, base, quote uint32) (orderBook *core.OrderBook, err error)
	CloseWallet(assetID uint32) error
	CreateWallet(appPass, walletPass []byte, form *core.WalletForm) error
	Exchanges() (exchanges map[string]*core.Exchange)
	InitializeClient(appPass []byte) error
	Login(appPass []byte) (*core.LoginResult, error)
	OpenWallet(assetID uint32, pw []byte) error
	GetFee(url, cert string) (fee uint64, err error)
	Register(form *core.RegisterForm) error
	Sync(dex string, base, quote uint32) (*core.OrderBook, *core.BookFeed, error)
	WalletState(assetID uint32) (walletState *core.WalletState)
	Wallets() (walletsStates []*core.WalletState)
}

// marketSyncer is used to synchronize market subscriptions. The marketSyncer
// manages a map of clients who are subscribed to the market, and distributes
// order book updates when received.
type marketSyncer struct {
	feed *core.BookFeed
	cl   *wsClient
}

// newMarketSyncer is the constructor for a marketSyncer.
func newMarketSyncer(ctx context.Context, cl *wsClient, feed *core.BookFeed) *dex.StartStopWaiter {
	ssWaiter := dex.NewStartStopWaiter(&marketSyncer{
		feed: feed,
		cl:   cl,
	})
	ssWaiter.Start(ctx)
	return ssWaiter
}

func (m *marketSyncer) Run(ctx context.Context) {
	defer m.feed.Close()
out:
	for {
		select {
		case update := <-m.feed.C:
			note, err := msgjson.NewNotification(update.Action, update)
			if err != nil {
				log.Errorf("error encoding notification message: %v", err)
				break out
			}
			err = m.cl.Send(note)
			if err != nil {
				log.Debug("send error. ending market feed")
				break out
			}
		case <-ctx.Done():
			break out
		}
	}
}

// RPCServer is a single-client http and websocket server enabling a JSON
// interface to the DEX client.
type RPCServer struct {
	ctx       context.Context
	core      ClientCore
	addr      string
	tlsConfig *tls.Config
	srv       *http.Server
	authsha   [32]byte
	mtx       sync.RWMutex
	syncers   map[string]*marketSyncer
	clients   map[int32]*wsClient
	wg        sync.WaitGroup
}

// genCertPair generates a key/cert pair to the paths provided.
func genCertPair(certFile, keyFile string) error {
	log.Infof("Generating TLS certificates...")

	org := "dcrdex autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, nil)
	if err != nil {
		return err
	}

	// Write cert and key files.
	if err = ioutil.WriteFile(certFile, cert, 0644); err != nil {
		return err
	}
	if err = ioutil.WriteFile(keyFile, key, 0600); err != nil {
		os.Remove(certFile)
		return err
	}

	log.Infof("Done generating TLS certificates")
	return nil
}

// writeJSON marshals the provided interface and writes the bytes to the
// ResponseWriter. The response code is assumed to be StatusOK.
func writeJSON(w http.ResponseWriter, thing interface{}) {
	writeJSONWithStatus(w, thing, http.StatusOK)
}

// writeJSONWithStatus marshals the provided interface and writes the bytes to the
// ResponseWriter with the specified response code.
func writeJSONWithStatus(w http.ResponseWriter, thing interface{}, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(thing); err != nil {
		log.Errorf("JSON encode error: %v", err)
	}
}

// handleJSON handles all https json requests.
func (s *RPCServer) handleJSON(w http.ResponseWriter, r *http.Request) {
	// All http routes are available over websocket too, so do not support
	// persistent http connections. Inform the user and close the connection
	// when response handling is completed.
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	r.Close = true

	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "error reading request body", http.StatusBadRequest)
		return
	}
	req := new(msgjson.Message)
	err = json.Unmarshal(body, req)
	if err != nil {
		http.Error(w, "JSON decode error", http.StatusUnprocessableEntity)
		return
	}
	if req.Type != msgjson.Request {
		http.Error(w, "Responses not accepted", http.StatusMethodNotAllowed)
		return
	}
	s.parseHTTPRequest(w, req)
}

// Config holds variables neede to create a new RPC Server.
type Config struct {
	Core                        ClientCore
	Addr, User, Pass, Cert, Key string
}

// SetLogger sets the logger for the RPCServer package.
func SetLogger(logger slog.Logger) {
	log = logger
}

// New is the constructor for an RPCServer.
func New(cfg *Config) (*RPCServer, error) {

	// Find or create the key pair.
	keyExists := fileExists(cfg.Key)
	certExists := fileExists(cfg.Cert)
	if certExists == !keyExists {
		return nil, fmt.Errorf("missing cert pair file")
	}
	if !keyExists && !certExists {
		err := genCertPair(cfg.Cert, cfg.Key)
		if err != nil {
			return nil, err
		}
	}
	keypair, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, err
	}

	// Prepare the TLS configuration.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{keypair},
		MinVersion:   tls.VersionTLS12,
	}

	// Create an HTTP router.
	mux := chi.NewRouter()
	httpServer := &http.Server{
		Handler:      mux,
		ReadTimeout:  rpcTimeoutSeconds * time.Second, // slow requests should not hold connections opened
		WriteTimeout: rpcTimeoutSeconds * time.Second, // hung responses must die
	}

	// Make the server.
	s := &RPCServer{
		core:      cfg.Core,
		srv:       httpServer,
		addr:      cfg.Addr,
		tlsConfig: tlsConfig,
		syncers:   make(map[string]*marketSyncer),
		clients:   make(map[int32]*wsClient),
	}

	// Create authsha to verify requests against.
	if cfg.User != "" && cfg.Pass != "" {
		login := cfg.User + ":" + cfg.Pass
		auth := "Basic " +
			base64.StdEncoding.EncodeToString([]byte(login))
		s.authsha = sha256.Sum256([]byte(auth))
	}

	// Middleware
	mux.Use(middleware.Recoverer)
	mux.Use(middleware.RealIP)
	mux.Use(s.authMiddleware)

	// Websocket endpoint
	mux.Get("/ws", s.handleWS)

	// https endpoint
	mux.Post("/", s.handleJSON)

	return s, nil
}

// Run starts the rpc server. Satisfies the dex.Runner interface.
func (s *RPCServer) Run(ctx context.Context) {
	// ctx passed to newMarketSyncer when making new market syncers.
	s.ctx = ctx

	// Create listener.
	listener, err := tls.Listen("tcp", s.addr, s.tlsConfig)
	if err != nil {
		log.Errorf("can't listen on %s. rpc server quitting: %v", s.addr, err)
		return
	}

	// Close the listener on context cancellation.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()

		if err := s.srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners:
			log.Errorf("HTTP server Shutdown: %v", err)
		}
	}()
	log.Infof("RPC server listening on %s", s.addr)
	if err := s.srv.Serve(listener); err != http.ErrServerClosed {
		log.Warnf("unexpected (http.Server).Serve error: %v", err)
	}
	s.mtx.Lock()
	for _, cl := range s.clients {
		cl.Disconnect()
	}
	s.mtx.Unlock()

	// Wait for market syncers to finish and Shutdown.
	s.wg.Wait()
	log.Infof("RPC server off")
}

var osExit = os.Exit

func createListener(protocol string, s *RPCServer) net.Listener {
	listener, err := tls.Listen(protocol, s.addr, s.tlsConfig)
	if err != nil {
		log.Errorf("can't listen on %s. rpc server quitting: %v", s.addr, err)
		osExit(1)
	}
	return listener
}

type RpcConn interface {
	Connect(ctx context.Context) (error, *sync.WaitGroup)
}

func NewRpcConn(s *RPCServer) RpcConn {
	return s
}

func (s *RPCServer) Start(ctx context.Context) error {
	// ctx passed to newMarketSyncer when making new market syncers.
	s.ctx = ctx

	rpcConn := NewRpcConn(s)

	connMaster := dex.NewConnectionMaster(rpcConn)
	err := connMaster.Connect(s.ctx)
	// If the initial connection returned an error, shut it down to kill the
	// auto-reconnect cycle.
	if err != nil {
		connMaster.Disconnect()
		return err
	}

	return nil
}

// Connect starts the rpc server. Satisfies the dex.Connector interface.
func (s *RPCServer) Connect(ctx context.Context) (error, *sync.WaitGroup) {
	//ctx passed to newMarketSyncer when making new market syncers.
	s.ctx = ctx

	//listener := createListener("tcp", s)

	//listener, err := connectListener("tcp", s)
	//if err != nil {
	//	return
	//}

	listener, err := tls.Listen("tcp", s.addr, s.tlsConfig)
	if err != nil {
		log.Errorf("can't listen on %s. rpc server quitting: %v", s.addr, err)
		return err, &s.wg
	}

	// Close the listener on context cancellation.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()

		if err := s.srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners:
			log.Errorf("HTTP server Shutdown: %v", err)
		}
	}()
	log.Infof("RPC server listening on %s", s.addr)
	if err := s.srv.Serve(listener); err != http.ErrServerClosed {
		log.Warnf("unexpected (http.Server).Serve error: %v", err)
	}
	s.mtx.Lock()
	for _, cl := range s.clients {
		cl.Disconnect()
	}
	s.mtx.Unlock()

	// Wait for market syncers to finish and Shutdown.
	s.wg.Wait()
	log.Infof("RPC server off")

	return nil, &s.wg
}

// handleRequest sends the request to the correct handler function if able.
func (s *RPCServer) handleRequest(req *msgjson.Message) *msgjson.ResponsePayload {
	payload := new(msgjson.ResponsePayload)
	if req.Route == "" {
		log.Debugf("route not specified")
		payload.Error = msgjson.NewError(msgjson.RPCUnknownRoute, "no route was supplied")
		return payload
	}

	// Find the correct handler for this route.
	h, exists := routes[req.Route]
	if !exists {
		log.Debugf("%v: %v", errUnknownCmd, req.Route)
		payload.Error = msgjson.NewError(msgjson.RPCUnknownRoute, errUnknownCmd.Error())
		return payload
	}

	params := new(RawParams)
	err := req.Unmarshal(params)
	if err != nil {
		log.Debugf("cannot unmarshal params for route %s", req.Route)
		payload.Error = msgjson.NewError(msgjson.RPCParseError, "unable to unmarshal request")
		return payload
	}

	return h(s, params)
}

// parseHTTPRequest parses the msgjson message in the request body, creates a
// response message, and writes it to the http.ResponseWriter.
func (s *RPCServer) parseHTTPRequest(w http.ResponseWriter, req *msgjson.Message) {
	payload := s.handleRequest(req)
	resp, err := msgjson.NewResponse(req.ID, payload.Result, payload.Error)
	if err != nil {
		msg := fmt.Sprintf("error encoding response: %v", err)
		http.Error(w, msg, http.StatusInternalServerError)
		log.Errorf("parseHTTPRequest: NewResponse failed: %s", msg)
		return
	}
	writeJSON(w, resp)
}

// authMiddleware checks incoming requests for authentication.
func (s *RPCServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header["Authorization"]
		if len(auth) == 0 || s.authsha != sha256.Sum256([]byte(auth[0])) {
			log.Warnf("authentication failure from ip: %s with auth: %s", r.RemoteAddr, auth)
			w.Header().Add("WWW-Authenticate", `Basic realm="dex RPC"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		log.Debugf("authenticated user with ip: %s", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	_, err := os.Stat(name)
	return !os.IsNotExist(err)
}

// Create a unique ID for a market.
func marketID(base, quote uint32) string {
	return strconv.Itoa(int(base)) + "_" + strconv.Itoa(int(quote))
}
