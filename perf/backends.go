package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const (
	ChildBackendEnvName            = "CHILD_BACKEND_NAME"
	ChildBackendTrafficTypeEnvName = "CHILD_BACKEND_TERMINATION_TYPE"
)

func serveBackendMetadata(backendsByTrafficType BackendsByTrafficType, port int, postNotifier func(b BoundBackend)) {
	// Provide synchronous access to the asynchronously registered
	// port number for a backend.
	var registeredBackends sync.Map

	printBackendsForType := func(w io.Writer, t TrafficType) error {
		for _, b := range backendsByTrafficType[t] {
			port, ok := registeredBackends.Load(b.Name)
			if !ok {
				panic("missing port for" + b.Name)
			}
			if _, err := io.WriteString(w, fmt.Sprintf("%v %v %v %v\n", b.HostAddr, b.Name, port, b.TrafficType)); err != nil {
				return err
			}
		}
		return nil
	}

	mux := http.NewServeMux()

	var mu sync.Mutex

	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method != "POST" {
			panic("unexpected: " + r.Method)
		}

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var x BoundBackend
		if err := decoder.Decode(&x); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		registeredBackends.Store(x.Name, x.Port)
		postNotifier(x)
	})

	mux.HandleFunc("/backends", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["json"]; ok {
			var boundBackendsByTrafficType = BoundBackendsByTrafficType{}

			for _, t := range AllTrafficTypes {
				for _, b := range backendsByTrafficType[t] {
					port, ok := registeredBackends.Load(b.Name)
					if !ok {
						panic("missing port for" + b.Name)
					}
					boundBackendsByTrafficType[t] = append(boundBackendsByTrafficType[t],
						BoundBackend{
							Backend: b,
							Port:    port.(int),
						})
				}
			}
			data, err := json.MarshalIndent(boundBackendsByTrafficType, "", "  ")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, err := io.WriteString(w, string(data)); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			for _, t := range AllTrafficTypes {
				if err := printBackendsForType(w, t); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
				}
			}
		}
	})

	mux.HandleFunc("/backends/edge", func(w http.ResponseWriter, r *http.Request) {
		if err := printBackendsForType(w, EdgeTraffic); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/backends/http", func(w http.ResponseWriter, r *http.Request) {
		if err := printBackendsForType(w, HTTPTraffic); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/backends/passthrough", func(w http.ResponseWriter, r *http.Request) {
		if err := printBackendsForType(w, PassthroughTraffic); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/backends/reencrypt", func(w http.ResponseWriter, r *http.Request) {
		if err := printBackendsForType(w, ReencryptTraffic); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})

	if err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", port), mux); err != nil {
		log.Fatal(err)
	}
}

func (c *ServeBackendsCmd) Run(p *ProgramCtx) error {
	hostIPAddr := HostIPAddress()
	backendsByTrafficType := BackendsByTrafficType{}

	for _, t := range AllTrafficTypes {
		for i := 0; i < p.Nbackends; i++ {
			backend := Backend{
				HostAddr:    hostIPAddr.String(),
				Name:        fmt.Sprintf("%s-%v-%v", p.HostPrefix, t, i),
				TrafficType: t,
			}
			backendsByTrafficType[t] = append(backendsByTrafficType[t], backend)
		}
	}

	log.SetPrefix(fmt.Sprintf("[P %v] ", os.Getpid()))

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGCHLD)
		log.Println(<-sigc)
		os.Exit(1)
	}()

	r, w, err := os.Pipe()
	if err != nil {
		return err
	}

	defer func(r *os.File) {
		_ = r.Close()
	}(r)
	defer func(w *os.File) {
		_ = w.Close()
	}(w)

	var backendsReady = make(chan bool)
	var backendsRegistered = 0

	go serveBackendMetadata(backendsByTrafficType, p.Port, func(b BoundBackend) {
		backendsRegistered += 1
		if backendsRegistered == len(backendsByTrafficType)*p.Nbackends {
			backendsReady <- true
			return
		}
	})

	log.Printf("starting %d backends for traffic types: %v\n",
		len(AllTrafficTypes)*p.Nbackends/len(AllTrafficTypes),
		AllTrafficTypes)

	for t, backends := range backendsByTrafficType {
		for _, backend := range backends {
			childEnv := []string{
				fmt.Sprintf("%s=%v", ChildBackendEnvName, backend.Name),
				fmt.Sprintf("%s=%v", ChildBackendTrafficTypeEnvName, t),
			}
			// We want to be a child of the current
			// process so the following fork/exec needs to
			// change the current program arguments so
			// that the exec, and subsequent command line
			// parsing, ensures we call serve-backend
			// (singular) and not server-backends
			// (plural). Otherwise we'll end up back here.
			newArgs := os.Args[:1]
			newArgs = append(newArgs, "serve-backend")
			args := append(newArgs, fmt.Sprintf("#%v", backend.Name))
			if _, err := syscall.ForkExec(args[0], args, &syscall.ProcAttr{
				Env:   append(os.Environ(), childEnv...),
				Files: []uintptr{0, 1, 2, r.Fd()},
			}); err != nil {
				return err
			}
		}
	}

	<-backendsReady
	log.Printf("%d backends registered", backendsRegistered)
	log.Printf("metadata server running at http://%s:%v/backends\n", Hostname(), p.Port)
	<-p.Context.Done()
	return nil
}
