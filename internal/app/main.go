package app

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bnvac/backoffice/internal/media"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Config struct {
	Addr         string
	DataDir      string
	Password     string
	Title        string
	BaseURL      string   // public address, for the URLs printed into QR codes
	ShopifyShops []string // storefronts to search, by hostname
	NexarID      string   // Octopart / Nexar OAuth client credentials
	NexarSecret  string
	NexarToken   string // a pasted access token; expires within a day
	// Thingiverse needs a free app token. Printables needs nothing, so model
	// search works out of the box either way.
	ThingiverseToken string
	AllowPrivate     bool // let URL imports reach LAN addresses
}

func configFromEnv() Config {
	c := Config{
		Addr:     env("PORT", "8080"),
		DataDir:  env("DATA_DIR", "./data"),
		Password: os.Getenv("AUTH_PASSWORD"),
		Title:    env("SITE_TITLE", "Backoffice"),
		BaseURL:  os.Getenv("BASE_URL"),
		// Credentials come from the environment and are never written to the
		// database, the templates, or the logs.
		NexarID:          os.Getenv("NEXAR_CLIENT_ID"),
		NexarSecret:      os.Getenv("NEXAR_CLIENT_SECRET"),
		NexarToken:       os.Getenv("NEXAR_TOKEN"),
		ThingiverseToken: os.Getenv("THINGIVERSE_TOKEN"),
	}
	c.ShopifyShops = defaultShopifyShops
	if raw := strings.TrimSpace(os.Getenv("SHOPIFY_SHOPS")); raw != "" {
		c.ShopifyShops = nil
		for _, shop := range strings.Split(raw, ",") {
			if shop = strings.TrimSpace(shop); shop != "" && shop != "none" {
				c.ShopifyShops = append(c.ShopifyShops, shop)
			}
		}
	}
	switch strings.ToLower(os.Getenv("ALLOW_PRIVATE_FETCH")) {
	case "1", "true", "yes":
		c.AllowPrivate = true
	}
	if !strings.Contains(c.Addr, ":") {
		c.Addr = ":" + c.Addr
	}
	return c
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type App struct {
	cfg    Config
	store  *Store
	photos *media.PhotoStore
	// Sliced G-code and firmware images, kept by hash so they can be sent or
	// flashed again. Separate folders because their size limits differ by
	// three orders of magnitude.
	gcode    *Blobs
	firmware *Blobs
	tmpl     *template.Template
	auth     *Auth
	fetcher  *Fetcher
	search   *SearchHub
	models   *ModelHub
	started  time.Time
	// Whether saving a part may go and look for its maker's logo in the
	// background. Off in tests, so a unit test never reaches the internet.
	autoLogos bool
	// The makers whose logo is being fetched right now. Saving five Espressif
	// parts in a row should fetch one logo, not five.
	fetching  map[string]bool
	fetchLock sync.Mutex
	// Maker logos, cached because every card on a grid asks for one.
	logos      map[string]string
	logosFresh bool
	logoMu     sync.RWMutex
}

// sessionKeyPath is where the HMAC key for sessions lives. It is needed after
// startup too, for the case where the first account is created on an install
// that never had a password.
func (a *App) sessionKeyPath() string {
	return filepath.Join(a.cfg.DataDir, "session.key")
}

// Run starts the server and blocks until it is asked to stop. It lives here
// rather than in cmd/backoffice so that everything it wires up stays
// unexported to the outside world.
func Run() {
	log.SetFlags(log.Ltime)
	cfg := configFromEnv()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("cannot create data dir %s: %v", cfg.DataDir, err)
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "inventory.db"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	photos, err := media.NewPhotoStore(filepath.Join(cfg.DataDir, "photos"))
	if err != nil {
		log.Fatalf("photo store: %v", err)
	}
	gcode, err := NewBlobs(filepath.Join(cfg.DataDir, "gcode"), maxGcode)
	if err != nil {
		log.Fatalf("gcode store: %v", err)
	}
	firmware, err := NewBlobs(filepath.Join(cfg.DataDir, "firmware"), maxFirmware)
	if err != nil {
		log.Fatalf("firmware store: %v", err)
	}
	store := &Store{db: db}
	auth, err := NewAuth(cfg.Password, filepath.Join(cfg.DataDir, "session.key"), store)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}

	fetcher := NewFetcher(cfg.AllowPrivate)
	app := &App{
		cfg:       cfg,
		store:     store,
		photos:    photos,
		gcode:     gcode,
		firmware:  firmware,
		tmpl:      mustTemplates(),
		auth:      auth,
		fetcher:   fetcher,
		search:    NewSearchHub(fetcher, cfg),
		models:    NewModelHub(fetcher, cfg),
		started:   time.Now(),
		autoLogos: true,
	}
	app.bindTemplates()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // uploads
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		mode := "open (no password set)"
		switch {
		case auth.Accounts():
			mode = "named accounts"
		case auth.Enabled():
			mode = "password protected"
		}
		fetch := "public hosts only"
		if cfg.AllowPrivate {
			fetch = "private addresses allowed"
		}
		log.Printf("%s listening on http://localhost%s  data=%s  auth=%s  url-import=%s  search=%s",
			cfg.Title, cfg.Addr, cfg.DataDir, mode, fetch, strings.Join(app.search.Sources(), ", "))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	// Public: health check and assets.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("GET /login", a.handleLoginForm)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)

	// Everything below requires a session when AUTH_PASSWORD is set.
	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", a.handleDashboard)
	protected.HandleFunc("GET /items", a.handleIndex)
	protected.HandleFunc("GET /items/new", a.handleNewForm)
	protected.HandleFunc("GET /items/{id}", a.handleItem)
	protected.HandleFunc("GET /items/{id}/edit", a.handleEditForm)
	protected.HandleFunc("POST /items", a.handleCreate)
	protected.HandleFunc("POST /items/{id}", a.handleUpdate)
	protected.HandleFunc("POST /items/{id}/delete", a.handleDelete)
	protected.HandleFunc("POST /items/{id}/quantity", a.handleQuantity)
	protected.HandleFunc("POST /items/{id}/photos", a.handleUploadPhotos)
	protected.HandleFunc("POST /photos/{id}/delete", a.handleDeletePhoto)
	protected.HandleFunc("POST /photos/{id}/cover", a.handleCoverPhoto)
	protected.HandleFunc("GET /media/thumb/{name}", a.handleThumb)
	protected.HandleFunc("GET /media/{name}", a.handleMedia)
	protected.HandleFunc("POST /items/{id}/photos/url", a.handlePhotoFromURL)
	protected.HandleFunc("POST /items/{id}/move", a.handleMoveItem)
	protected.HandleFunc("GET /export.csv", a.handleExportCSV)

	// Folders
	protected.HandleFunc("GET /folders/{id}", a.handleFolder)
	protected.HandleFunc("POST /folders", a.handleCreateFolder)
	protected.HandleFunc("POST /folders/{id}", a.handleRenameFolder)
	protected.HandleFunc("POST /folders/{id}/delete", a.handleDeleteFolder)

	// Prices
	protected.HandleFunc("POST /items/{id}/price", a.handleSetPrice)
	protected.HandleFunc("POST /items/{id}/price/delete", a.handleDeletePrice)

	// Manufacturers, and their logos.
	protected.HandleFunc("GET /manufacturers", a.handleManufacturers)
	protected.HandleFunc("POST /manufacturers/logo", a.handleFetchLogo)
	protected.HandleFunc("POST /manufacturers/logo/clear", a.handleClearLogo)
	protected.HandleFunc("POST /manufacturers/merge", a.handleMergeManufacturer)

	// Onboarding, the profile, and the workshop it describes.
	protected.HandleFunc("GET /welcome", a.handleWelcome)
	protected.HandleFunc("POST /welcome/finish", a.handleFinishOnboarding)
	protected.HandleFunc("GET /profile", a.handleProfile)
	protected.HandleFunc("POST /profile", a.handleSaveProfile)
	protected.HandleFunc("GET /workshop", a.handleWorkshop)
	protected.HandleFunc("POST /workshop/interpret", a.handleParseTools)
	protected.HandleFunc("POST /workshop/confirm", a.handleConfirmTools)
	protected.HandleFunc("POST /workshop/tools", a.handleAddTool)
	protected.HandleFunc("POST /workshop/tools/{id}", a.handleUpdateTool)
	protected.HandleFunc("POST /workshop/tools/{id}/delete", a.handleDeleteTool)

	// The printers, and what they will take. Separate from the tool list
	// because a printer is a specification, not a name on a shelf.
	protected.HandleFunc("GET /printing", a.handlePrinting)
	protected.HandleFunc("POST /printing/printers", a.handleAddPrinter)
	protected.HandleFunc("POST /printing/printers/{id}", a.handleUpdatePrinter)
	protected.HandleFunc("POST /printing/printers/{id}/delete", a.handleDeletePrinter)
	protected.HandleFunc("GET /printing/material/{key}", a.handleMaterial)
	protected.HandleFunc("GET /printing/status", a.handlePrinterStatus)
	protected.HandleFunc("POST /printing/printers/{id}/send", a.handleSendGcode)
	protected.HandleFunc("POST /printing/jobs/{id}/resend", a.handleResendJob)
	protected.HandleFunc("GET /printing/jobs/{id}/file", a.handleJobFile)
	protected.HandleFunc("POST /printing/jobs/{id}/return", a.handleReturnFilament)

	// One printer: maintenance by print hours, and the spares kept for it.
	protected.HandleFunc("GET /printing/printers/{id}", a.handlePrinter)
	protected.HandleFunc("POST /printing/printers/{id}/maintenance", a.handleAddMaint)
	protected.HandleFunc("POST /printing/printers/{id}/hours", a.handleBaseHours)
	protected.HandleFunc("POST /printing/printers/{id}/spares", a.handleSpare)
	protected.HandleFunc("POST /maintenance/{id}/done", a.handleDoneMaint)
	protected.HandleFunc("POST /maintenance/{id}/delete", a.handleDeleteMaint)

	// Filament, in grams, by colour.
	protected.HandleFunc("GET /filament", a.handleFilament)
	protected.HandleFunc("POST /filament", a.handleAddSpool)
	protected.HandleFunc("GET /filament/{id}", a.handleSpool)
	protected.HandleFunc("POST /filament/{id}", a.handleUpdateSpool)
	protected.HandleFunc("POST /filament/{id}/{action}", a.handleSpoolAction)

	// What is on each board. Recording, not flashing: the board is plugged
	// into the machine you are sitting at, not this one.
	protected.HandleFunc("GET /firmware", a.handleFirmware)
	protected.HandleFunc("POST /firmware", a.handleAddFlash)
	protected.HandleFunc("GET /firmware/{id}", a.handleFlash)
	protected.HandleFunc("GET /firmware/{id}/file", a.handleFlashFile)
	protected.HandleFunc("POST /firmware/{id}/delete", a.handleDeleteFlash)

	// The shelf, switched on.
	protected.HandleFunc("GET /machines", a.handleMachines)
	protected.HandleFunc("GET /machines/{id}", a.handleMachine)
	protected.HandleFunc("POST /machines", a.handleAddMachine)
	protected.HandleFunc("POST /machines/{id}", a.handleUpdateMachine)
	protected.HandleFunc("POST /machines/{id}/delete", a.handleDeleteMachine)

	// Where a model gets plugged in, if one does.
	protected.HandleFunc("GET /settings/ai", a.handleAISettings)
	protected.HandleFunc("POST /settings/ai", a.handleAddProvider)
	protected.HandleFunc("POST /settings/ai/{id}", a.handleUpdateProvider)
	protected.HandleFunc("POST /settings/ai/{id}/use", a.handleActivateProvider)
	protected.HandleFunc("POST /settings/ai/{id}/test", a.handleTestProvider)
	protected.HandleFunc("POST /settings/ai/{id}/key/clear", a.handleClearProviderKey)
	protected.HandleFunc("POST /settings/ai/{id}/delete", a.handleDeleteProvider)

	// The idea board.
	protected.HandleFunc("GET /ideas", a.handleBrainstorm)
	protected.HandleFunc("POST /ideas/generate", a.handleGenerateIdeas)
	protected.HandleFunc("GET /ideas/{id}", a.handleIdea)
	protected.HandleFunc("POST /ideas/{id}/cases", a.handleIdeaCases)
	protected.HandleFunc("POST /ideas/{id}/status", a.handleIdeaStatus)
	protected.HandleFunc("POST /ideas/{id}/plan", a.handlePlanIdea)
	protected.HandleFunc("POST /ideas/{id}/delete", a.handleDeleteIdea)

	// Tags
	protected.HandleFunc("GET /tags", a.handleTags)
	protected.HandleFunc("POST /tags/icon", a.handleSetTagIcon)

	// Projects: a bill of materials plus what the shelf cannot cover.
	protected.HandleFunc("GET /projects", a.handleProjects)
	protected.HandleFunc("POST /projects", a.handleCreateProject)
	protected.HandleFunc("GET /projects/{id}", a.handleProject)
	protected.HandleFunc("POST /projects/{id}", a.handleUpdateProject)
	protected.HandleFunc("POST /projects/{id}/delete", a.handleDeleteProject)
	protected.HandleFunc("POST /projects/{id}/parts", a.handleAddProjectPart)
	protected.HandleFunc("POST /project-parts/{id}/delete", a.handleDeleteProjectPart)

	// Datasheets, pinouts and manuals.
	protected.HandleFunc("POST /items/{id}/refs", a.handleAddReference)
	protected.HandleFunc("POST /refs/{id}/delete", a.handleDeleteReference)
	protected.HandleFunc("POST /items/{id}/price/refresh", a.handleRefreshPrice)
	protected.HandleFunc("POST /items/{id}/octopart", a.handleEnrichFromOctopart)

	// Labels: a QR code opens the item on a phone, a barcode feeds a scanner.
	protected.HandleFunc("GET /labels", a.handleLabels)
	protected.HandleFunc("GET /labels/{what}", a.handleThingLabels)
	protected.HandleFunc("GET /qr/{kind}/{id}", a.handleThingQR)
	protected.HandleFunc("GET /items/{id}/qr.png", a.handleItemQR)
	protected.HandleFunc("GET /items/{id}/barcode.png", a.handleItemBarcode)

	// One-step add from a pasted link, and type-ahead over the inventory.
	protected.HandleFunc("POST /items/quick", a.handleQuickAdd)
	protected.HandleFunc("GET /items/search.json", a.handleLiveSearch)

	// Building a project: reserve the parts, then take them off the shelf.
	protected.HandleFunc("POST /projects/{id}/consume", a.handleConsumeProject)
	protected.HandleFunc("POST /projects/{id}/return", a.handleReturnProject)

	// Bills of materials, in and out.
	protected.HandleFunc("POST /projects/{id}/bom", a.handleImportBOM)
	protected.HandleFunc("GET /projects/{id}/bom.csv", a.handleExportBOM)
	protected.HandleFunc("GET /projects/{id}/shopping-list.csv", a.handleShortfallCSV)

	// The lab notebook.
	protected.HandleFunc("POST /projects/{id}/log", a.handleAddLogEntry)

	// The checklist. Every one of these posts back to the project page, so
	// ticking a box works with JavaScript off.
	protected.HandleFunc("POST /projects/{id}/steps", a.handleAddSteps)
	protected.HandleFunc("POST /projects/{id}/steps/reset", a.handleResetSteps)
	protected.HandleFunc("POST /steps/{id}/toggle", a.handleToggleStep)
	protected.HandleFunc("POST /steps/{id}/edit", a.handleUpdateStep)
	protected.HandleFunc("POST /steps/{id}/move/{dir}", a.handleMoveStep)
	protected.HandleFunc("POST /steps/{id}/delete", a.handleDeleteStep)
	protected.HandleFunc("POST /log/{id}/delete", a.handleDeleteLogEntry)

	// Sub-assemblies and the pin map.
	protected.HandleFunc("POST /projects/{id}/sub", a.handleAddSubAssembly)
	protected.HandleFunc("POST /projects/{id}/pins", a.handleAddPin)
	protected.HandleFunc("POST /pins/{id}/delete", a.handleDeletePin)
	protected.HandleFunc("GET /projects/{id}/wiring.csv", a.handleWiringCSV)

	// Orders: what you bought, and putting it on the shelf when it turns up.
	protected.HandleFunc("GET /orders", a.handleOrders)
	protected.HandleFunc("POST /orders", a.handleCreateOrder)
	protected.HandleFunc("GET /orders/{id}", a.handleOrder)
	protected.HandleFunc("POST /orders/{id}", a.handleUpdateOrder)
	protected.HandleFunc("POST /orders/{id}/delete", a.handleDeleteOrder)
	protected.HandleFunc("POST /orders/{id}/lines", a.handleAddOrderLine)
	protected.HandleFunc("POST /order-lines/{id}/delete", a.handleDeleteOrderLine)
	protected.HandleFunc("POST /orders/{id}/receive", a.handleReceiveOrder)
	protected.HandleFunc("POST /orders/{id}/unreceive", a.handleUnreceiveOrder)
	protected.HandleFunc("POST /projects/{id}/order", a.handleOrderFromShortfall)

	// Reading the labels back: a phone camera pointed at a drawer.
	protected.HandleFunc("GET /scan", a.handleScan)
	protected.HandleFunc("GET /lookup", a.handleLookup)
	protected.HandleFunc("POST /lookup", a.handleLookup)

	// Documentation and land patterns.
	protected.HandleFunc("POST /items/{id}/docs", a.handleFindDocs)
	protected.HandleFunc("POST /items/{id}/footprint", a.handleSetFootprint)

	// Cases and mounts somebody has already printed.
	// Prints: the per-item case hunt, and a plain search for everything else.
	protected.HandleFunc("GET /prints", a.handlePrints)
	protected.HandleFunc("POST /prints/keep", a.handleKeepPrint)
	protected.HandleFunc("GET /items/{id}/models", a.handleModelSearch)
	protected.HandleFunc("POST /items/{id}/models", a.handleAttachModel)
	protected.HandleFunc("POST /models/{id}/mesh", a.handleModelMesh)
	protected.HandleFunc("POST /models/{id}/delete", a.handleDeleteModel)
	protected.HandleFunc("GET /media/remote", a.handleRemoteImage)

	// Bench calculators, wired to what is on the shelf.
	protected.HandleFunc("GET /tools", a.handleTools)

	// Health, backup, accounts and the audit trail.
	protected.HandleFunc("GET /admin", a.handleAdmin)
	protected.HandleFunc("GET /admin/backup.zip", a.handleBackup)
	protected.HandleFunc("POST /admin/restore", a.handleRestore)
	protected.HandleFunc("POST /admin/sweep", a.handleSweep)
	protected.HandleFunc("POST /admin/thumbs", a.handleRebuildThumbs)
	protected.HandleFunc("POST /admin/prune", a.handlePruneAudit)
	protected.HandleFunc("POST /users", a.handleCreateUser)
	protected.HandleFunc("POST /users/{id}", a.handleUpdateUser)
	protected.HandleFunc("POST /users/{id}/delete", a.handleDeleteUser)
	protected.HandleFunc("GET /activity", a.handleActivity)

	// Look a part up by name, or read a URL the person pasted.
	protected.HandleFunc("POST /import/search", a.handleSearch)
	protected.HandleFunc("POST /import/preview", a.handleImportPreview)

	mux.Handle("/", a.auth.Require(protected))
	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/media/") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// bindTemplates swaps in the template functions that have to read from the app
// itself. They cannot go in templateFuncs, which runs before the app exists.
func (a *App) bindTemplates() {
	a.tmpl = a.tmpl.Funcs(template.FuncMap{"makerLogo": a.logoFor})
}

func mustTemplates() *template.Template {
	return template.Must(template.New("").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html"))
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"since": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return plural(int(d.Minutes()), "minute") + " ago"
			case d < 24*time.Hour:
				return plural(int(d.Hours()), "hour") + " ago"
			case d < 30*24*time.Hour:
				return plural(int(d.Hours()/24), "day") + " ago"
			default:
				return t.Local().Format("2 Jan 2006")
			}
		},
		"add":   func(a, b int) int { return a + b },
		"money": func(amount float64) string { return formatMoney(amount, "USD") },

		// dict lets a page pass several named values into a shared partial.
		"dict": func(pairs ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				if k, ok := pairs[i].(string); ok {
					m[k] = pairs[i+1]
				}
			}
			return m
		},

		// query rebuilds the grid URL with one filter changed and the rest
		// preserved, so chips compose instead of resetting each other. Passing
		// the value a filter already holds clears it, which makes every chip a
		// toggle without needing a second helper.
		"query": func(q Query, key, value string) template.URL {
			return template.URL(q.URL(key, value))
		},
		// toggleTag adds or removes one tag, leaving the other tags in place.
		"toggleTag": func(q Query, tag string) template.URL {
			return template.URL(q.WithTagToggled(tag))
		},
		"toggleIO": func(q Query, name string) template.URL {
			return template.URL(q.WithInterfaceToggled(name))
		},
		"hasIO": func(q Query, name string) bool {
			for _, i := range q.Interfaces {
				if strings.EqualFold(i, name) {
					return true
				}
			}
			return false
		},
		// section marks the nav link for the part of the app you are in.
		"section": func(p page, prefix string) bool {
			return p.Path == prefix || strings.HasPrefix(p.Path, prefix+"/")
		},

		// The maker's mark. monogram and hue are the fallback for a maker with
		// no logo; makerLogo is replaced with the real lookup by bindTemplates,
		// and stands here only so the templates parse.
		"monogram": monogram,
		"hue": func(name string) int {
			return Manufacturer{Name: name}.Hue()
		},
		"makerLogo": func(string) string { return "" },
		"ioIcon":    IconForInterface,
		"slice":     func(v ...string) []string { return v },

		// bomCSV renders a parsed bill of materials back into the canonical CSV
		// this app's own importer reads. The review step posts that instead of
		// keeping the upload in server-side state, so confirming re-parses and
		// re-matches exactly what was shown.
		"bomCSV": bomCSV,
		"hasTag": func(q Query, tag string) bool {
			for _, t := range q.Tags {
				if strings.EqualFold(t, tag) {
					return true
				}
			}
			return false
		},

		// initials is the placeholder shown for items with no photo yet.
		"initials": func(name string) string {
			var out []rune
			for _, word := range strings.Fields(name) {
				out = append(out, []rune(strings.ToUpper(word))[0])
				if len(out) == 2 {
					break
				}
			}
			return string(out)
		},
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
