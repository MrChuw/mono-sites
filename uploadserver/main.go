package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/handlers"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)


func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
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
	if err := os.MkdirAll(config.UploadDir, 0755); err != nil {
		log.Fatalf("Failed to create upload dir: %v", err)
	}
	if err := os.MkdirAll(config.TrashDir, 0755); err != nil {
		log.Fatalf("Failed to create trash dir: %v", err)
	}

	umami.NewInstance()

	client, err := initDB()
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	sqlDB, _ := client.DB()
	defer sqlDB.Close()

	if err := client.AutoMigrate(&db.APIKey{}, &db.Tag{}, &db.UploadedFile{}, &db.DeletedFileLog{}); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	if err := utils.PurgeTrashOnStartup(client); err != nil {
		log.Printf("Startup purge error: %v", err)
	}

	utils.StartBackgroundTasks(client)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(handlers.CloudflareMiddleware)

	r.Get("/", handlers.RootHandler)
	r.Get("/favicon.ico", handlers.FaviconHandler(client))
	r.Get("/api/delete/{token}", handlers.DeleteFileHandler(client))
	r.Get("/{file_path:.*}", handlers.ServeFileHandler(client))
	// r.HandleFunc("/*", handlers.serveFileHandler(client))

	r.Route("/api", func(api chi.Router) {
		api.Use(handlers.AuthMiddleware(client))

		api.Post("/upload", handlers.UploadFileHandler(client, true))
		api.Post("/uploaddoxx", handlers.UploadFileHandler(client, false))
		// api.Get("/delete/{token}", handlers.deleteFileHandler(client))
		api.Post("/keys", handlers.CreateKeyHandler(client))
		api.Get("/metrics/user", handlers.GetUserMetricsHandler(client))
		api.Get("/metrics/admin", handlers.GetAdminMetricsHandler(client))
		api.Post("/sharex/config", handlers.SharexConfigHandler(client))
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		handlers.ServeFileHandler(client)(w, r)
	})

	port := config.Port
	if port == "" {
		port = "8000"
	}
	log.Printf("Starting server on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func initDB() (*gorm.DB, error) {
	dbPath := config.DatabaseURL
	if len(dbPath) > 10 && dbPath[:10] == "sqlite:///" {
		dbPath = dbPath[10:]
	}
	if dir := filepath.Dir(dbPath); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	return gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
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

	// Connect to DB to create the key
	client, err := initDB()
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	sqlDB, _ := client.DB()
	defer sqlDB.Close()

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
	if err := client.Create(&newKey).Error; err != nil {
		log.Fatalf("Failed to create key: %v", err)
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
