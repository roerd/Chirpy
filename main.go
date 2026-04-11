package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/roerd/Chirpy/internal/database"

	_ "github.com/lib/pq"
)

func main() {
	godotenv.Load()
	dbURL := os.Getenv("DB_URL")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal("Error connecting to database:", err)
	}
	defer db.Close()

	apiCfg := &apiConfig{}
	apiCfg.dbQueries = database.New(db)
	apiCfg.platform = os.Getenv("PLATFORM")

	mux := http.NewServeMux()
	mux.Handle("/app/", apiCfg.middlewareMetricsInc(http.StripPrefix("/app/", http.FileServer(http.Dir(".")))))
	mux.HandleFunc("GET /api/healthz", healthz)
	mux.HandleFunc("GET /admin/metrics", apiCfg.metrics)
	mux.HandleFunc("POST /admin/reset", apiCfg.reset)
	mux.HandleFunc("POST /api/chirps", apiCfg.createChirp)
	mux.HandleFunc("GET /api/chirps", apiCfg.getAllChirps)
	mux.HandleFunc("POST /api/users", apiCfg.createUser)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) error {
	response, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(response)
	return nil
}

func respondWithError(w http.ResponseWriter, code int, msg string) error {
	return respondWithJSON(w, code, map[string]string{"error": msg})
}

func cleanBody(body string) string {
	// Remove words that are not allowed
	forbiddenWords := []string{"kerfuffle", "sharbert", "fornax"}
	replacement := "****"
	words := strings.Split(body, " ")
	for i, word := range words {
		for _, forbidden := range forbiddenWords {
			if strings.EqualFold(word, forbidden) {
				words[i] = replacement
			}
		}
	}
	cleanedBody := strings.Join(words, " ")
	return cleanedBody
}

func validateChirp(w http.ResponseWriter, body string) bool {
	if len(body) > 140 {
		if err := respondWithError(w, http.StatusBadRequest, "Chirp is too long"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return false
	}

	return true
}

type Chirp struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (cfg *apiConfig) createChirp(w http.ResponseWriter, r *http.Request) {
	type request struct {
		UserID uuid.UUID `json:"user_id"`
		Body   string    `json:"body"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	if !validateChirp(w, req.Body) {
		return
	}

	cleanedBody := cleanBody(req.Body)

	dbChirp, err := cfg.dbQueries.CreateChirp(r.Context(), database.CreateChirpParams{
		UserID: req.UserID,
		Body:   cleanedBody,
	})
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error creating chirp: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	chirp := Chirp{
		ID:        dbChirp.ID,
		UserID:    dbChirp.UserID,
		Body:      dbChirp.Body,
		CreatedAt: dbChirp.CreatedAt,
		UpdatedAt: dbChirp.UpdatedAt,
	}

	if err := respondWithJSON(w, http.StatusCreated, chirp); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) getAllChirps(w http.ResponseWriter, r *http.Request) {
	dbChirps, err := cfg.dbQueries.GetAllChirps(r.Context())
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error fetching chirps: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	chirps := make([]Chirp, len(dbChirps))
	for i, dbChirp := range dbChirps {
		chirps[i] = Chirp{
			ID:        dbChirp.ID,
			UserID:    dbChirp.UserID,
			Body:      dbChirp.Body,
			CreatedAt: dbChirp.CreatedAt,
			UpdatedAt: dbChirp.UpdatedAt,
		}
	}

	if err := respondWithJSON(w, http.StatusOK, chirps); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

type User struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Email     string    `json:"email"`
}

func (cfg *apiConfig) createUser(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Email string `json:"email"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	dbUser, err := cfg.dbQueries.CreateUser(r.Context(), req.Email)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error creating user: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	user := User{
		ID:        dbUser.ID,
		CreatedAt: dbUser.CreatedAt,
		UpdatedAt: dbUser.UpdatedAt,
		Email:     dbUser.Email,
	}

	if err := respondWithJSON(w, http.StatusCreated, user); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

type apiConfig struct {
	fileserverHits atomic.Int32
	dbQueries      *database.Queries
	platform       string
}

func (cfg *apiConfig) middlewareMetricsInc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg.fileserverHits.Add(1)
		next.ServeHTTP(w, r)
	})
}

func (cfg *apiConfig) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(fmt.Appendf(nil, `<html>
  <body>
    <h1>Welcome, Chirpy Admin</h1>
    <p>Chirpy has been visited %d times!</p>
  </body>
</html>`, cfg.fileserverHits.Load()))
}

func (cfg *apiConfig) reset(w http.ResponseWriter, r *http.Request) {
	if cfg.platform != "dev" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	cfg.fileserverHits.Store(0)
	cfg.dbQueries.DeleteUsers(r.Context())

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
