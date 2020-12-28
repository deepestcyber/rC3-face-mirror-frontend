package main

import (
	"crypto/subtle"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type Pool struct {
	pool map[string]struct{}
	lock sync.Mutex
}

func (p *Pool) Put(id string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.pool[id] = struct{}{}
}

// Select an id from the pool, preferring the
// ones inserted first but randomly skipping
// (50% chance of skip)
func (p *Pool) Get() (id string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if len(p.pool) == 0 {
		return ""
	}

	for id = range p.pool {
		if rand.Intn(2) == 1 {
			continue
		}
	}

	delete(p.pool, id)
	return
}

type ComputeHandlers struct {
	handlers map[string]*ComputeHandler
	lock     sync.RWMutex
	pool     Pool // pool of available handler ids
}

func (c *ComputeHandlers) Add(name string, conn *websocket.Conn) string {
	id := fmt.Sprintf("%s%d", name, time.Now().UnixNano())

	handler := &ComputeHandler{name, conn}

	c.lock.Lock()
	defer c.lock.Unlock()

	c.handlers[id] = handler
	c.pool.Put(id)

	return id
}

// the pool might include ids of nodes which have disconnected/errored
// so we need to make sure that the handler is still registered with
// us and may try to get another one.
func (c *ComputeHandlers) Get() (string, *ComputeHandler) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	for {
		id := c.pool.Get()
		if id == "" {
			log.Println("no id in pool :(")
			return "", nil
		}

		handler, ok := c.handlers[id]

		if !ok {
			continue
		}

		log.Println("A worker acquired worked", id)
		return id, handler
	}
}

// Mark the handler as free again to be used by others.
func (c *ComputeHandlers) Free(id string) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	if _, ok := c.handlers[id]; ok {
		log.Println("Marked", id, "as free again")
		c.pool.Put(id)
	}
}

func (c *ComputeHandlers) RemoveById(id string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	log.Println("Worker", id, "was removed")
	delete(c.handlers, id)
}

type ComputeHandler struct {
	Name string
	Conn *websocket.Conn
}

var computeHandlers *ComputeHandlers

// FIXME load from config
const TOKEN = "supersecretsauce"

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

const CLIENT_TIMEOUT = 10 * time.Second

func indexHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "html/frontend.html")
}

func resetWorker(handler *ComputeHandler) {
	log.Println("sending reset to", handler.Name)
	handler.Conn.WriteMessage(websocket.TextMessage, []byte("reset"))
}

func noWorkersHandler(w http.ResponseWriter) {
	http.Error(w, "No workers available", 501)
}

func inputStreamHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	defer conn.Close()

	handlerId, handler := computeHandlers.Get()

	if handler == nil {
		log.Println("had to reject a client; no workers available")
		noWorkersHandler(w)
		return
	}

	defer computeHandlers.Free(handlerId)

	// in every case reset the worker when we are done here.
	defer resetWorker(handler)

	// the client websocket can basically lock the connection
	// for an infinite amount of time. make sure that it does
	// not by having a timer that is reset after each receive
	clientTimer := time.AfterFunc(CLIENT_TIMEOUT, func() {
		log.Println("Closing client connection due to timeout.")
		conn.Close()
	})

	defer clientTimer.Stop()

	for {
		t0 := time.Now()
		tClientReceiveA := t0

		messageType, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}

		clientTimer.Reset(CLIENT_TIMEOUT)

		tClientReceiveB := time.Since(tClientReceiveA)

		if messageType != websocket.TextMessage {
			log.Println("Invalid message type")
			return
		}

		tHandlerSendA := time.Now()
		handler.Conn.WriteMessage(websocket.BinaryMessage, p)
		tHandlerSendB := time.Since(tHandlerSendA)

		tHandlerReceivedA := time.Now()
		_, p, err = handler.Conn.ReadMessage()
		if err != nil {
			log.Println("Compute handler failed:", err)
			computeHandlers.RemoveById(handlerId)
			return
		}

		tHandlerReceivedB := time.Since(tHandlerReceivedA)

		tClientSendA := time.Now()

		//log.Println("received answer from compute node; relaying")
		err = conn.WriteMessage(websocket.TextMessage, p)

		if err != nil {
			log.Println(err)
			return
		}

		tClientSendB := time.Since(tClientSendA)
		tTotal := time.Since(t0)

		log.Println("Total took", tTotal)
		log.Println("Client receive took", tClientReceiveB)
		log.Println("Write to computation took", tHandlerSendB)
		log.Println("Computation took", tHandlerReceivedB)
		log.Println("Client write took", tClientSendB)
	}
}

func validateToken(tok string) bool {
	if len(tok) != len(TOKEN) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(TOKEN)) == 1
}

func assertPermission(w http.ResponseWriter, vars map[string]string) bool {
	valid := validateToken(vars["token"])

	if !valid {
		http.Error(w, "Not authorized", 401)
	}
	return valid
}

func registerComputeHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	log.Println("registerComputeHandler: Attempting to register new node")
	if !assertPermission(w, vars) {
		return
	}

	log.Println("registerComputeHandler: authenticated")

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	id := computeHandlers.Add(vars["name"], conn)
	log.Println("registerComputeHandler: id is", id)
}

func main() {
	computeHandlers = &ComputeHandlers{
		handlers: make(map[string]*ComputeHandler),
		pool: Pool{
			pool: make(map[string]struct{}),
		},
	}

	router := mux.NewRouter()
	router.StrictSlash(true)

	router.HandleFunc("/", indexHandler)
	router.PathPrefix("/static/").Handler(
		http.StripPrefix("/static/", http.FileServer(http.Dir("html"))))
	router.HandleFunc("/inputStream", inputStreamHandler)
	router.HandleFunc("/registerCompute/{name}/{token}", registerComputeHandler)

	http.Handle("/", router)

	log.Fatal(http.ListenAndServe(":8080", nil))
}
