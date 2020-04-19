package comms

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"decred.org/dcrdex/dex"
	"decred.org/dcrdex/dex/msgjson"
	"github.com/decred/dcrd/certgen"
	"github.com/decred/slog"
	"github.com/gorilla/websocket"
)

func makeRequest(id uint64, route string, msg interface{}) *msgjson.Message {
	req, _ := msgjson.NewRequest(id, route, msg)
	return req
}

// genCertPair generates a key/cert pair to the paths provided.
func genCertPair(certFile, keyFile string, altDNSNames []string) error {
	log.Infof("Generating TLS certificates...")

	org := "dcrdex autogenerated cert"
	validUntil := time.Now().Add(10 * 365 * 24 * time.Hour)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, altDNSNames)
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

func TestMain(m *testing.M) {
	backendLogger := slog.NewBackend(os.Stdout)
	defer os.Stdout.Sync()
	log := backendLogger.Logger("Debug")
	log.SetLevel(slog.LevelTrace)
	UseLogger(log)
	os.Exit(m.Run())
}

func TestWsConn(t *testing.T) {
	// Must wait for goroutines, especially the ones that capture t.
	var wg sync.WaitGroup
	defer wg.Wait()

	upgrader := websocket.Upgrader{}

	pingCh := make(chan struct{})
	readPumpCh := make(chan interface{})
	writePumpCh := make(chan *msgjson.Message)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pingWait := time.Millisecond * 200

	var wsc *wsConn

	var id uint64
	// server's "/ws" handler
	handler := func(w http.ResponseWriter, r *http.Request) {
		id := atomic.AddUint64(&id, 1) // shadow id
		hCtx, hCancel := context.WithCancel(ctx)

		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("unable to upgrade http connection: %s", err)
		}

		c.SetPongHandler(func(string) error {
			t.Logf("handler #%d: pong received", id)
			return nil
		})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-pingCh:
					err := c.WriteControl(websocket.PingMessage, []byte{},
						time.Now().Add(writeWait))
					if err != nil {
						t.Errorf("handler #%d: ping error: %v", id, err)
						return
					}

					t.Logf("handler #%d: ping sent", id)

				case msg := <-readPumpCh:
					err := c.WriteJSON(msg)
					if err != nil {
						t.Errorf("handler #%d: write error: %v", id, err)
						return
					}

				case <-hCtx.Done():
					return
				}
			}
		}()

		for {
			mType, message, err := c.ReadMessage()
			if err != nil {
				c.Close()
				hCancel()

				// If the context has been canceled, don't do anything.
				if hCtx.Err() != nil {
					return
				}

				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					// Terminate on a normal close message.
					return
				}

				t.Fatalf("handler #%d: read error: %v\n", id, err)
				return
			}

			if mType == websocket.TextMessage {
				msg, err := msgjson.DecodeMessage(message)
				if err != nil {
					t.Errorf("handler #%d: decode error: %v", id, err)
					c.Close()
					hCancel()
					return
				}

				writePumpCh <- msg
			}
		}
	}

	certFile, err := ioutil.TempFile("", "certfile")
	if err != nil {
		t.Fatalf("unable to create temp certfile: %s", err)
	}
	certFile.Close()
	defer os.Remove(certFile.Name())

	keyFile, err := ioutil.TempFile("", "keyfile")
	if err != nil {
		t.Fatalf("unable to create temp keyfile: %s", err)
	}
	keyFile.Close()
	defer os.Remove(keyFile.Name())

	err = genCertPair(certFile.Name(), keyFile.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}

	certB, err := ioutil.ReadFile(certFile.Name())
	if err != nil {
		t.Fatalf("file reading error: %v", err)
	}

	host := "127.0.0.1:6060"
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handler)

	server := &http.Server{
		WriteTimeout: time.Second * 5,
		ReadTimeout:  time.Second * 5,
		IdleTimeout:  time.Second * 5,
		Addr:         host,
		Handler:      mux,
	}
	defer server.Shutdown(context.Background())

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := server.ListenAndServeTLS(certFile.Name(), keyFile.Name())
		if err != nil {
			fmt.Println(err)
		}
	}()

	cfg := &WsCfg{
		URL:      "wss://" + host + "/ws",
		PingWait: pingWait,
		Cert:     certB,
	}
	conn, err := NewWsConn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wsc = conn.(*wsConn)
	waiter := dex.NewConnectionMaster(wsc)
	err = waiter.Connect(ctx)
	t.Log("Connect:", err)

	reconnectAndPing := func() {
		// Drop the connection and force a reconnect by waiting.
		time.Sleep(pingWait * 2)

		// Wait for a reconnection.
		for !wsc.isConnected() {
			time.Sleep(time.Millisecond * 10)
			continue
		}

		// Send a ping.
		pingCh <- struct{}{}
	}

	orderid, _ := hex.DecodeString("ceb09afa675cee31c0f858b94c81bd1a4c2af8c5947d13e544eef772381f2c8d")
	matchid, _ := hex.DecodeString("7c6b44735e303585d644c713fe0e95897e7e8ba2b9bba98d6d61b70006d3d58c")
	match := &msgjson.Match{
		OrderID:  orderid,
		MatchID:  matchid,
		Quantity: 20,
		Rate:     2,
		Address:  "DsiNAJCd2sSazZRU9ViDD334DaLgU1Kse3P",
	}

	// Ensure a malformed message to the client does not terminate
	// the connection.
	readPumpCh <- []byte("{notjson")

	// Send a message to the client.
	sent := makeRequest(1, msgjson.MatchRoute, match)
	readPumpCh <- sent

	// Fetch the read source.
	readSource := wsc.MessageSource()
	if readSource == nil {
		t.Fatal("expected a non-nil read source")
	}

	// Read the message received by the client.
	received := <-readSource

	// Ensure the received message equal to the sent message.
	if received.Type != sent.Type {
		t.Fatalf("expected %v type, got %v", sent.Type, received.Type)
	}

	if received.Route != sent.Route {
		t.Fatalf("expected %v route, got %v", sent.Route, received.Route)
	}

	if received.ID != sent.ID {
		t.Fatalf("expected %v id, got %v", sent.ID, received.ID)
	}

	if !bytes.Equal(received.Payload, sent.Payload) {
		t.Fatal("sent and received payload mismatch")
	}

	reconnectAndPing()

	coinID := []byte{
		0xc3, 0x16, 0x10, 0x33, 0xde, 0x09, 0x6f, 0xd7, 0x4d, 0x90, 0x51, 0xff,
		0x0b, 0xd9, 0x9e, 0x35, 0x9d, 0xe3, 0x50, 0x80, 0xa3, 0x51, 0x10, 0x81,
		0xed, 0x03, 0x5f, 0x54, 0x1b, 0x85, 0x0d, 0x43, 0x00, 0x00, 0x00, 0x0a,
	}

	contract, _ := hex.DecodeString("caf8d277f80f71e4")
	init := &msgjson.Init{
		OrderID:  orderid,
		MatchID:  matchid,
		CoinID:   coinID,
		Contract: contract,
	}

	// Send a message from the client.
	mId := wsc.NextID()
	sent = makeRequest(mId, msgjson.InitRoute, init)
	handlerRun := false
	err = wsc.Request(sent, func(*msgjson.Message) {
		handlerRun = true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read the message received by the server.
	received = <-writePumpCh

	// Ensure the received message equal to the sent message.
	if received.Type != sent.Type {
		t.Fatalf("expected %v type, got %v", sent.Type, received.Type)
	}

	if received.Route != sent.Route {
		t.Fatalf("expected %v route, got %v", sent.Route, received.Route)
	}

	if received.ID != sent.ID {
		t.Fatalf("expected %v id, got %v", sent.ID, received.ID)
	}

	if !bytes.Equal(received.Payload, sent.Payload) {
		t.Fatal("sent and received payload mismatch")
	}

	// Ensure the next id is as expected.
	next := wsc.NextID()
	if next != 2 {
		t.Fatalf("expected next id to be %d, got %d", 2, next)
	}

	// Ensure the request got logged.
	hndlr := wsc.respHandler(mId)
	if hndlr == nil {
		t.Fatalf("no handler found")
	}
	hndlr.f(nil)
	if !handlerRun {
		t.Fatalf("wrong handler retrieved")
	}

	// Lookup an unlogged request id.
	hndlr = wsc.respHandler(next)
	if hndlr != nil {
		t.Fatal("expected an error for unlogged id")
	}

	waiter.Disconnect()

	select {
	case _, ok := <-readSource:
		if ok {
			t.Error("read source should have been closed")
		}
	default:
		t.Error("read source should have been closed")
	}
}
