package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	// Logging
	"github.com/unrolled/logger"

	// Stats/Metrics
	"github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/exp"
	"github.com/thoas/stats"

	rice "github.com/GeertJohan/go.rice"
	"github.com/NYTimes/gziphandler"
	"github.com/julienschmidt/httprouter"
)

var (
	client = &http.Client{
		Timeout: 5 * time.Second,
	}
)

// Counters ...
type Counters struct {
	r metrics.Registry
}

func NewCounters() *Counters {
	counters := &Counters{
		r: metrics.NewRegistry(),
	}
	return counters
}

func (c *Counters) Inc(name string) {
	metrics.GetOrRegisterCounter(name, c.r).Inc(1)
}

func (c *Counters) Dec(name string) {
	metrics.GetOrRegisterCounter(name, c.r).Dec(1)
}

func (c *Counters) IncBy(name string, n int64) {
	metrics.GetOrRegisterCounter(name, c.r).Inc(n)
}

func (c *Counters) DecBy(name string, n int64) {
	metrics.GetOrRegisterCounter(name, c.r).Dec(n)
}

// Server ...
type Server struct {
	bind      string
	config    Config
	templates *Templates
	router    *httprouter.Router
	server    *http.Server

	// Logger
	logger *logger.Logger

	// Stats/Metrics
	counters *Counters
	stats    *stats.Stats
}

func (s *Server) render(name string, w http.ResponseWriter, ctx interface{}) {
	buf, err := s.templates.Exec(name, ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	if buf != nil {
		_, err = buf.WriteTo(w)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// IndexHandler ...
func (s *Server) IndexHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		var (
			q    string
			cmd  string
			args []string
		)

		s.counters.Inc("n_index")

		// Query ?q=
		q = r.URL.Query().Get("q")

		// Form name=q
		if q == "" {
			q = r.FormValue("q")
		}

		if q != "" {
			tokens := strings.Split(q, " ")
			if len(tokens) > 0 {
				cmd, args = tokens[0], tokens[1:]
			}
		} else {
			cmd = p.ByName("command")
			args = strings.Split(p.ByName("args"), "/")
		}

		if cmd == "" {
			s.render("index", w, nil)
		} else {
			if command := LookupCommand(cmd); command != nil {
				err := command.Exec(w, r, args)
				if err != nil {
					http.Error(
						w,
						fmt.Sprintf(
							"Error processing command %s: %s",
							command.Name(), err,
						),
						http.StatusInternalServerError,
					)
				}
				if err := AddHistoryEntry(command.Name(), ""); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			} else if bookmark, ok := LookupBookmark(cmd); ok {
				q := strings.Join(args, " ")
				bookmark.Exec(w, r, q)
				if err := AddHistoryEntry(bookmark.Name(), q); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			} else {
				if s.config.URL != "" {
					url := s.config.URL
					if q != "" {
						url = fmt.Sprintf(url, q)
					}
					http.Redirect(w, r, url, http.StatusFound)
				} else {
					http.Error(
						w,
						fmt.Sprintf("Invalid Command: %v", cmd),
						http.StatusBadRequest,
					)
				}
			}
		}
	}
}

// HelpHandler ...
func (s *Server) HelpHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		s.counters.Inc("n_help")

		s.render("help", w, nil)
	}
}

// ListHandler ...
func (s *Server) ListHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		s.counters.Inc("n_list")

		var (
			bk  []Bookmark
			cmd []Command
		)

		prefix := []byte("bookmark_")
		err := db.Scan(prefix, func(key []byte) error {
			val, err := db.Get(key)
			if err != nil {
				return err
			}
			name := strings.TrimPrefix(string(key), "bookmark_")
			bk = append(bk, Bookmark{name, string(val)})
			return nil
		})
		if err != nil {
			log.Printf("error reading list of bookmarks: %s", err)
		}

		var names []string
		for k := range commands {
			names = append(names, k)
		}
		sort.Sort(sort.StringSlice(names))
		for _, name := range names {
			cmd = append(cmd, commands[name])
		}

		data := map[string]interface{}{
			"Bookmarks": bk,
			"Commands":  cmd,
		}
		s.render("list", w, data)
	}
}

// HistoryHandler shows commands usage history
func (s *Server) HistoryHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		s.counters.Inc("n_history")
		allEntries := make([]HistoryEntry, 0)
		err := db.Scan([]byte("history_"), func(key []byte) error {
			val, err := db.Get(key)
			if err != nil {
				s.logger.Println(err)
				return nil
			}
			var entry HistoryEntry
			err = json.Unmarshal(val, &entry)
			if err != nil {
				s.logger.Println(err)
				return nil
			}
			allEntries = append(allEntries, entry)
			return nil
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		allEntriesReversed := make([]HistoryEntry, 0)
		for i := len(allEntries) - 1; i >= 0; i-- {
			entry := allEntries[i]
			allEntriesReversed = append(allEntriesReversed, entry)
		}
		entries := make([]HTMLHistoryEntry, 0)
		for _, entry := range allEntriesReversed {
			entries = append(entries, HTMLHistoryEntry{
				When: time.Unix(0, entry.Timestamp).Format(time.StampMilli),
				What: fmt.Sprintf("%s %s", entry.Command, entry.Value),
			})
		}
		s.render("history", w, map[string]interface{}{"Entries": entries})
	}
}

// OpenSearchHandler ...
func (s *Server) OpenSearchHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		s.counters.Inc("n_opensearch")

		w.Header().Set("Content-Type", "text/xml")
		w.Write(
			[]byte(fmt.Sprintf(
				OpenSearchTemplate,
				s.config.Title,
				s.config.FQDN,
				s.config.FQDN,
			)),
		)
	}
}

// SuggestionsHandler ...
func (s *Server) SuggestionsHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		// Query ?q=
		q := r.URL.Query().Get("q")
		resp, err := client.Get(fmt.Sprintf(s.config.SuggestURL, url.QueryEscape(q)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode > 200 {
			http.Error(w, "request failed", resp.StatusCode)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := io.Copy(w, resp.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// StatsHandler ...
func (s *Server) StatsHandler() httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		bs, err := json.Marshal(s.stats.Data())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		w.Write(bs)
	}
}

// Shutdown ...
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.server.Shutdown(ctx); err != nil {
		log.Printf("error shutting down server: %s", err)
		return err
	}

	if err := db.Close(); err != nil {
		log.Printf("error closing store: %s", err)
		return err
	}

	return nil
}

// Run ...
func (s *Server) Run() (err error) {
	idleConnsClosed := make(chan struct{})
	go func() {
		sigch := make(chan os.Signal, 1)
		signal.Notify(sigch, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigch
		log.Printf("Received signal %s", sig)

		log.Printf("Shutting down...")

		// We received an interrupt signal, shut down.
		if err = s.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Fatalf("Error shutting down HTTP server: %s", err)
		}
		close(idleConnsClosed)
	}()

	if err = s.ListenAndServe(); err != http.ErrServerClosed {
		// Error starting or closing listener:
		log.Fatalf("HTTP server ListenAndServe: %s", err)
	}

	<-idleConnsClosed

	return
}

// ListenAndServe ...
func (s *Server) ListenAndServe() error {
	return s.server.ListenAndServe()
}

func (s *Server) initRoutes() {
	s.router.Handler("GET", "/debug/metrics", exp.ExpHandler(s.counters.r))
	s.router.GET("/debug/stats", s.StatsHandler())

	s.router.GET("/", s.IndexHandler())
	s.router.POST("/", s.IndexHandler())
	s.router.GET("/help", s.HelpHandler())
	s.router.GET("/list", s.ListHandler())
	s.router.GET("/history", s.HistoryHandler())
	s.router.GET("/opensearch.xml", s.OpenSearchHandler())
	s.router.GET("/suggest", s.SuggestionsHandler())
}

// NewServer ...
func NewServer(bind string, config Config) (*Server, error) {
	router := httprouter.New()

	server := &Server{
		bind:      bind,
		config:    config,
		router:    router,
		templates: NewTemplates("base"),

		server: &http.Server{
			Addr: bind,
			Handler: logger.New(logger.Options{
				Prefix:               "golinks",
				RemoteAddressHeaders: []string{"X-Forwarded-For"},
			}).Handler(
				gziphandler.GzipHandler(
					router,
				),
			),
		},

		// Logger
		logger: logger.New(logger.Options{
			Prefix:               "golinks",
			RemoteAddressHeaders: []string{"X-Forwarded-For"},
			OutputFlags:          log.LstdFlags,
		}),

		// Stats/Metrics
		counters: NewCounters(),
		stats:    stats.New(),
	}

	// Templates
	box := rice.MustFindBox("templates")

	indexTemplate := template.New("index")
	template.Must(indexTemplate.Parse(box.MustString("index.html")))
	template.Must(indexTemplate.Parse(box.MustString("base.html")))

	helpTemplate := template.New("help")
	template.Must(helpTemplate.Parse(box.MustString("help.html")))
	template.Must(helpTemplate.Parse(box.MustString("base.html")))

	listTemplate := template.New("list")
	template.Must(listTemplate.Parse(box.MustString("list.html")))
	template.Must(listTemplate.Parse(box.MustString("base.html")))

	historyTemplate := template.New("history")
	template.Must(historyTemplate.Parse(box.MustString("history.html")))
	template.Must(historyTemplate.Parse(box.MustString("base.html")))

	server.templates.Add("index", indexTemplate)
	server.templates.Add("help", helpTemplate)
	server.templates.Add("list", listTemplate)
	server.templates.Add("history", historyTemplate)

	server.initRoutes()

	return server, nil
}
