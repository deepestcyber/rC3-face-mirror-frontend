package main

import (
	"crypto/subtle"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"sync"
)

type ComputeHandlers struct {
	Handlers []*ComputeHandler
	lock     sync.RWMutex
}

func (c *ComputeHandlers) Add(name string, conn *websocket.Conn) {
	handler := &ComputeHandler{name, conn}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.Handlers = append(c.Handlers, handler)
}

func (c *ComputeHandlers) RemoveByIndex(i int) {
	if len(c.Handlers) <= i {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	c.Handlers = append(c.Handlers[:i], c.Handlers[i+1:]...)
}

func (c *ComputeHandlers) GetByIndex(i int) *ComputeHandler {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.Handlers[i]
}

type ComputeHandler struct {
	Name string
	Conn *websocket.Conn
}

var computeHandlers *ComputeHandlers

// FIXME load from config
const TOKEN = "supersecretsauce"


func indexHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "html/frontend.html")
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func inputStreamHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	defer conn.Close()

	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}

		if messageType != websocket.TextMessage {
			log.Println("Invalid message type")
			return
		}

		//log.Println(messageType, p)
		handler := computeHandlers.GetByIndex(0)
		handler.Conn.WriteMessage(websocket.BinaryMessage, p)
		_, p, err = handler.Conn.ReadMessage()

		if err != nil {
			log.Println("Compute handler failed:", err)
			computeHandlers.RemoveByIndex(0)
			return
		}

		//log.Println("received answer from compute node; relaying")
		err = conn.WriteMessage(websocket.TextMessage, p)

		if err != nil {
			log.Println(err)
			return
		}
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

	computeHandlers.Add(vars["name"], conn)
}

func main() {
	computeHandlers = &ComputeHandlers{
		Handlers: make([]*ComputeHandler, 0),
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
