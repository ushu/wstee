package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
)

var (
	WSURL    string // address of the remote server
	BindAddr string // (local) address to bind for incoming connections
	SpyAddr  string // (local) address to bind for spy connections
	Quiet    bool
	Verbose  bool
)

// IncomingMessage holds the info about a WebSocket message
type IncomingMessage struct {
	Type int
	Data []byte
}

// Upgrader handles websocket handshake for incoming connections
var Upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// ConnectionEvent is emitted on connect/disconnet
type ConnectionEvent struct {
	Conn         *websocket.Conn
	IsDisconnect bool
}

// Dialer handles the websocket client protocol
var Dialer = websocket.DefaultDialer

func init() {
	flag.StringVar(&BindAddr, "bind", ":2222", "address for forwarded connections")
	flag.StringVar(&SpyAddr, "spy", ":3333", "address for spy connections")
	flag.BoolVar(&Quiet, "quiet", false, "disable all ouptut")
	flag.BoolVar(&Verbose, "verbose", false, "enable verbose output")
	flag.Usage = usage
	log.SetPrefix("[wstee] ")
}

func main() {
	flag.Parse()
	ctx := context.Background()

	// ensure we have an address
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}
	WSURL = flag.Arg(0)
	// avoid both quiet and verbose ...
	if Quiet {
		Verbose = false
	}

	err := startServer(ctx)
	if err != nil {
		if err != context.Canceled && !Quiet {
			log.Println(err)
		}
		if err != context.Canceled {
			os.Exit(1)
		}
	}
}

func startServer(ctx context.Context) error {
	if !Quiet {
		log.Println("starting server")
	}

	// listen for Ctrl+C
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// and cancel the context if clicked
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		if !Quiet {
			log.Println("stopping server")
		}
		cancel()
	}()

	return bindAllAndWait(ctx, BindAddr, SpyAddr, WSURL)
}

// the mail "loop": bind ports and wait for events. should not return. ever.
func bindAllAndWait(ctx context.Context, bindAddr, spyAddr, serverAddr string) error {
	// we create servers for receiving connections
	bConn := make(chan ConnectionEvent)
	bServer := prepareServer(ctx, bindAddr, bConn)
	defer bServer.Close()
	sConn := make(chan ConnectionEvent)
	sServer := prepareServer(ctx, spyAddr, sConn)
	defer sServer.Close()

	// and start listening
	done := make(chan error, 3)
	listen := func(server *http.Server, addr string) {
		if !Quiet {
			log.Println("binding", addr)
		}
		select {
		case done <- server.ListenAndServe():
		case <-ctx.Done():
		}
	}
	go listen(bServer, bindAddr)
	go listen(sServer, spyAddr)

	// while listening, we want to process incoming connections
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // when we leave this function, we want to stop the event-processing loop below

	// we maintain the list of active spy connections
	var sConnections []*websocket.Conn
	// as well as a channel for incoming payloads
	in := make(chan IncomingMessage)

	// loop while processing events
	go func() {
		for {
			select {
			case ev := <-bConn:
				if !ev.IsDisconnect {
					// new incoming connection: connect remove server
					go dial(ctx, ev.Conn, serverAddr, in)
				}
			case ev := <-sConn:
				// keep track of currently-active spy connections
				sConnections = updateConnections(ev, sConnections)
			case p := <-in:
				// re-dispatch incoming messages to spy connections
				if Verbose && len(sConnections) > 0 {
					log.Println("copying message to spy connection(s)")
				}
				for _, conn := range sConnections {
					if err := conn.WriteMessage(p.Type, p.Data); err != nil && !Quiet {
						log.Println("could not write to spy connection:", err)
					}
				}
			case <-ctx.Done():
				done <- ctx.Err()
				break
			}
		}
	}()

	// wait for any server to stop
	return <-done
}

// connects to the remote server, and forwards messages
func dial(ctx context.Context, conn *websocket.Conn, serverAddr string, in chan IncomingMessage) error {
	// connect to the remote server
	dconn, _, err := Dialer.DialContext(ctx, serverAddr, nil)
	if err != nil {
		if !Quiet {
			log.Println("failed connecting to", WSURL)
		}
		conn.Close()
		return err
	}

	// local cancelation context
	ctx, cancel := context.WithCancel(ctx)

	// load all the incoming messages
	client := make(chan IncomingMessage)
	server := make(chan IncomingMessage)
	go func() {
		pumpMessages(ctx, conn, client) // load all from conn into "client"
		cancel()                        // client connection closed: stop the goroutine
	}()
	go func() {
		pumpMessages(ctx, dconn, server) // load all from dconn into "server"
		cancel()                         // server connection closed: stop the goroutine
	}()

	// copy all messages to "in"
	client = tee(ctx, client, in)
	server = tee(ctx, server, in)

	// and forward them: server -> client & client -> server
	go pushMessages(ctx, conn, server)
	go pushMessages(ctx, dconn, client)

	<-ctx.Done()
	return ctx.Err()
}

func prepareServer(ctx context.Context, addr string, connections chan ConnectionEvent) *http.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Upgrader.Upgrade(w, r, nil)
		if err != nil {
			if !Quiet {
				log.Println("Error upgrading to WebSockets:", err)
			}
			return
		}

		// push connection
		if Verbose {
			log.Println("new connection on", addr)
		}
		connections <- ConnectionEvent{conn, false}

		// auto-send disconnect event
		conn.SetCloseHandler(func(code int, text string) error {
			if Verbose {
				log.Println("lost connection on", addr)
			}
			connections <- ConnectionEvent{conn, true}
			return nil
		})
	})
	return &http.Server{
		Addr:           addr,
		Handler:        handler,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
}

func updateConnections(ev ConnectionEvent, connections []*websocket.Conn) []*websocket.Conn {
	if ev.IsDisconnect {
		res := make([]*websocket.Conn, 0, len(connections))
		for _, c := range connections {
			if c != ev.Conn {
				res = append(res, c)
			}
		}
		return res
	}
	return append(connections, ev.Conn)
}

func pushMessages(ctx context.Context, conn *websocket.Conn, ch <-chan IncomingMessage) {
	for {
		select {
		case p := <-ch:
			if err := conn.WriteMessage(p.Type, p.Data); err != nil && !Quiet {
				log.Println("write error:", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func pumpMessages(ctx context.Context, conn *websocket.Conn, ch chan<- IncomingMessage) {
	for {
		select {
		default:
			messageType, p, err := conn.ReadMessage()
			if err != nil {
				if !Quiet {
					log.Println("connection closed:", err)
				}
				return
			}
			ch <- IncomingMessage{messageType, p}
		case <-ctx.Done():
			return
		}
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, "Usage: wstee [OPTIONS] URL")
	fmt.Fprintln(os.Stderr, "OPTIONS:")
	flag.PrintDefaults()
}

func tee(ctx context.Context, in chan IncomingMessage, out chan<- IncomingMessage) chan IncomingMessage {
	ch := make(chan IncomingMessage)
	go func() {
		defer close(ch)
		for {
			select {
			case m, ok := <-in:
				if !ok {
					break
				}
				ch <- m
				out <- m
			case m := <-ch:
				in <- m
				out <- m
			case <-ctx.Done():
				break
			}
		}
	}()
	return ch
}
