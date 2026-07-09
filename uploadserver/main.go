package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/barasher/go-exiftool"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"

	"database/sql"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/extra/bundebug"
	_ "modernc.org/sqlite"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/handlers"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func main() {
	if err := godotenv.Load(); err != nil {
		slog.Info("No .env file found, using environment variables")
	}
	config.LoadConfig()

	var showHelp bool
	flag.BoolVar(&showHelp, "h", false, "Show help")
	flag.BoolVar(&showHelp, "help", false, "Show help")
	flag.Usage = mainUsage
	flag.Parse()

	if showHelp {
		flag.Usage()
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		startServer()
		return
	}

	switch args[0] {
	case "serve":
		startServer()
	case "create-key":
		handleCreateKey(args[1:])
	case "help":
		flag.Usage()
	default:
		fmt.Printf("Unknown command: %s\n\n", args[0])
		flag.Usage()
		os.Exit(1)
	}
}

func startServer() {
	utils.InitLogger(config.Environment)

	dirs := []string{
		config.UploadDir,
		config.TrashDir,
		config.ThumbDir,
		filepath.Join(config.ThumbDir, "t"),
		filepath.Join(config.ThumbDir, "p"),
		filepath.Join(config.ThumbDir, "w"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("Failed to create directory", "path", dir, "error", err)
			os.Exit(1)
		}
	}

	umami.NewInstance()

	ctx := context.Background()
	dbConn, err := initDB()
	if err != nil {
		slog.Error("Failed to connect database", "error", err)
		os.Exit(1)
	}
	defer dbConn.Close()

	if err := db.MigrateDB(ctx, dbConn); err != nil {
		slog.Error("Migration failed", "error", err)
		os.Exit(1)
	}

	if config.Environment == "debug" {
		dbConn.AddQueryHook(bundebug.NewQueryHook(
			bundebug.WithVerbose(true),
		))
	}

	if err := utils.PurgeTrashOnStartup(ctx, dbConn); err != nil {
		slog.Error("Startup purge error", "error", err)
	}

	utils.ExifDaemon, err = exiftool.NewExiftool(
		exiftool.ClearFieldsBeforeWriting(),
	)
	if err != nil {
		slog.Error("Failed to start exiftool daemon", "error", err)
		os.Exit(1)
	}
	defer utils.ExifDaemon.Close()

	utils.StartBackgroundTasks(ctx, dbConn)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(handlers.CloudflareMiddleware)

	r.Get("/", handlers.RootHandler)
	r.Get("/favicon.ico", handlers.FaviconHandler(dbConn))
	r.Get("/api/delete/{token}", handlers.DeleteFileHandler(dbConn))
	r.Get("/{file_path:.*}", handlers.ServeFileHandler(dbConn))
	// r.HandleFunc("/*", handlers.serveFileHandler(dbConn))

	r.Route("/api", func(api chi.Router) {
		api.Use(handlers.AuthMiddleware(dbConn))

		api.Post("/upload", handlers.UploadFileHandler(dbConn, true))
		api.Post("/uploaddoxx", handlers.UploadFileHandler(dbConn, false))
		// api.Get("/delete/{token}", handlers.deleteFileHandler(dbConn))
		api.Post("/keys", handlers.CreateKeyHandler(dbConn))
		api.Get("/metrics/user", handlers.GetUserMetricsHandler(dbConn))
		api.Get("/metrics/admin", handlers.GetAdminMetricsHandler(dbConn))
		api.Post("/sharex/config", handlers.SharexConfigHandler(dbConn))
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		handlers.ServeFileHandler(dbConn)(w, r)
	})

	port := config.Port
	if port == "" {
		port = "8000"
	}
	slog.Info("Starting server", "port", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func initDB() (*bun.DB, error) {
	dbPath := config.DatabaseURL
	if len(dbPath) > 10 && dbPath[:10] == "sqlite:///" {
		dbPath = dbPath[10:]
	}
	if dir := filepath.Dir(dbPath); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	db := bun.NewDB(sqldb, sqlitedialect.New())
	return db, nil
}

func handleCreateKey(args []string) {
	fs := flag.NewFlagSet("create-key", flag.ExitOnError)
	fs.Usage = createKeyUsage

	owner := fs.String("owner", "", "Owner name")
	roleStr := fs.String("role", "normal", "Role (owner, vip, normal)")
	maxSizeMB := fs.Float64("max-size-mb", 0, "Max upload size in MB")

	fs.Parse(args)

	if *owner == "" {
		fmt.Println("Error: --owner is required")
		fs.Usage()
		os.Exit(1)
	}

	// Map role
	role := db.RoleNormal
	switch *roleStr {
	case "owner":
		role = db.RoleOwner
	case "vip":
		role = db.RoleVIP
	case "normal":
		role = db.RoleNormal
	default:
		fmt.Printf("Invalid role '%s', using normal.\n", *roleStr)
	}

	dbConn, err := initDB()
	if err != nil {
		slog.Error("Failed to connect database", "error", err)
		os.Exit(1)
	}
	defer dbConn.Close()
	ctx := context.Background()

	key := "sk_" + utils.SecureRandomString(48)
	var maxSize *int64
	if *maxSizeMB > 0 {
		mb := int64(*maxSizeMB * 1024 * 1024)
		maxSize = &mb
	}

	newKey := db.APIKey{
		Key:           key,
		Owner:         *owner,
		Role:          role,
		MaxUploadSize: maxSize,
	}
	_, err = dbConn.NewInsert().
		Model(&newKey).
		Exec(ctx)
	if err != nil {
		slog.Error("Failed to create keye", "error", err)
		os.Exit(1)
	}

	fmt.Printf("\nAPI Key Created:\nOwner: %s\nRole: %s\nKey: %s\n\n", newKey.Owner, newKey.Role, newKey.Key)
}

func mainUsage() {
	fmt.Println(`UploadServer - A lightweight file upload server with API key authentication.

Usage:
  uploadserver [command]

Available Commands:
  serve       Start the HTTP server (default)
  create-key  Create a new API key
  help        Show this help message

Flags:
  -h, --help  Show this help message

Use "uploadserver [command] --help" for more information about a command.`)
}

func createKeyUsage() {
	fmt.Println(`Create a new API key.

Usage:
 uploadserver create-key --owner NAME [options]

Options:
 --owner string
       Owner name. (required)

 --role string
       Permission level: owner, vip, normal. (default "normal")

 --max-size-mb float
       Maximum upload size in MB. 0 means no limit.

Example:
 uploadserver create-key --owner "John Doe" --role vip --max-size-mb 500`)
}
