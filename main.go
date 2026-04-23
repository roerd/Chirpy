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
	"github.com/roerd/Chirpy/internal/auth"
	"github.com/roerd/Chirpy/internal/database"

	_ "github.com/lib/pq"
)

type apiConfig struct {
	fileserverHits atomic.Int32
	dbQueries      *database.Queries
	platform       string
	jwtSecret      string
	polkaKey       string
}

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
	apiCfg.jwtSecret = os.Getenv("JWT_SECRET")
	apiCfg.polkaKey = os.Getenv("POLKA_KEY")

	mux := http.NewServeMux()
	mux.Handle("/app/", apiCfg.middlewareMetricsInc(http.StripPrefix("/app/", http.FileServer(http.Dir(".")))))
	mux.HandleFunc("GET /api/healthz", healthz)
	mux.HandleFunc("GET /admin/metrics", apiCfg.metrics)
	mux.HandleFunc("POST /admin/reset", apiCfg.reset)
	mux.HandleFunc("POST /api/login", apiCfg.login)
	mux.HandleFunc("POST /api/refresh", apiCfg.refresh)
	mux.HandleFunc("POST /api/revoke", apiCfg.revoke)
	mux.HandleFunc("POST /api/chirps", apiCfg.createChirp)
	mux.HandleFunc("GET /api/chirps", apiCfg.getAllChirps)
	mux.HandleFunc("GET /api/chirps/{chirpID}", apiCfg.getChirpByID)
	mux.HandleFunc("DELETE /api/chirps/{chirpID}", apiCfg.deleteChirp)
	mux.HandleFunc("POST /api/users", apiCfg.createUser)
	mux.HandleFunc("PUT /api/users", apiCfg.updateUser)
	mux.HandleFunc("POST /api/polka/webhooks", apiCfg.polkaWebhooks)

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

func (cfg *apiConfig) login(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	dbUser, err := cfg.dbQueries.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := respondWithError(w, http.StatusUnauthorized, "Incorrect email or password"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error fetching user: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	match, err := auth.CheckPasswordHash(req.Password, dbUser.HashedPassword)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error checking password: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}
	if !match {
		if err := respondWithError(w, http.StatusUnauthorized, "Incorrect email or password"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	token, err := auth.MakeJWT(dbUser.ID, cfg.jwtSecret, time.Hour)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error generating token: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	refreshToken := auth.MakeRefreshToken()
	cfg.dbQueries.CreateRefreshToken(r.Context(), database.CreateRefreshTokenParams{
		Token:     refreshToken,
		UserID:    dbUser.ID,
		ExpiresAt: time.Now().Add(60 * 24 * time.Hour), // Refresh token valid for 7 days
	})

	type response struct {
		ID           uuid.UUID `json:"id"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
		Email        string    `json:"email"`
		IsChirpyRed  bool      `json:"is_chirpy_red"`
		Token        string    `json:"token"`
		RefreshToken string    `json:"refresh_token"`
	}

	resp := response{
		ID:           dbUser.ID,
		CreatedAt:    dbUser.CreatedAt,
		UpdatedAt:    dbUser.UpdatedAt,
		Email:        dbUser.Email,
		IsChirpyRed:  dbUser.IsChirpyRed,
		Token:        token,
		RefreshToken: refreshToken,
	}

	if err := respondWithJSON(w, http.StatusOK, resp); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) refresh(w http.ResponseWriter, r *http.Request) {
	refreshToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	dbToken, err := cfg.dbQueries.GetRefreshToken(r.Context(), refreshToken)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := respondWithError(w, http.StatusUnauthorized, "Invalid refresh token"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error fetching refresh token: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	if dbToken.RevokedAt.Valid || dbToken.ExpiresAt.Before(time.Now()) {
		if err := respondWithError(w, http.StatusUnauthorized, "Refresh token is expired or revoked"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	token, err := auth.MakeJWT(dbToken.UserID, cfg.jwtSecret, time.Hour)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error generating token: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	type response struct {
		Token string `json:"token"`
	}

	resp := response{
		Token: token,
	}

	if err := respondWithJSON(w, http.StatusOK, resp); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) revoke(w http.ResponseWriter, r *http.Request) {
	refreshToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	_, err = cfg.dbQueries.RevokeRefreshToken(r.Context(), refreshToken)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := respondWithError(w, http.StatusUnauthorized, "Invalid refresh token"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error revoking refresh token: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type Chirp struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (cfg *apiConfig) createChirp(w http.ResponseWriter, r *http.Request) {
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Invalid or expired token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	type request struct {
		Body string `json:"body"`
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
		UserID: userID,
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
	s := r.URL.Query().Get("author_id")
	var authorID uuid.UUID
	var err error
	if s != "" {
		authorID, err = uuid.Parse(s)
		if err != nil {
			if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid author_id: %v", err)); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
	} else {
		authorID = uuid.Nil
	}

	var dbChirps []database.Chirp
	if authorID != uuid.Nil {
		dbChirps, err = cfg.dbQueries.GetChirpsByUserID(r.Context(), authorID)
	} else {
		dbChirps, err = cfg.dbQueries.GetAllChirps(r.Context())
	}
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

func (cfg *apiConfig) getChirpByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("chirpID")
	chirpID, err := uuid.Parse(idStr)
	if err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chirp ID: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	dbChirp, err := cfg.dbQueries.GetChirpByID(r.Context(), chirpID)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := respondWithError(w, http.StatusNotFound, "Chirp not found"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error fetching chirp: %v", err)); err != nil {
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

	if err := respondWithJSON(w, http.StatusOK, chirp); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) deleteChirp(w http.ResponseWriter, r *http.Request) {
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Invalid or expired token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	idStr := r.PathValue("chirpID")
	chirpID, err := uuid.Parse(idStr)
	if err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chirp ID: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	chirp, err := cfg.dbQueries.GetChirpByID(r.Context(), chirpID)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := respondWithError(w, http.StatusNotFound, "Chirp not found"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error fetching chirp: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	if chirp.UserID != userID {
		if err := respondWithError(w, http.StatusForbidden, "You can only delete your own chirps"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	err = cfg.dbQueries.DeleteChirp(r.Context(), chirpID)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error deleting chirp: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type User struct {
	ID          uuid.UUID `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Email       string    `json:"email"`
	IsChirpyRed bool      `json:"is_chirpy_red"`
}

func (cfg *apiConfig) createUser(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	password := req.Password
	if len(password) < 5 {
		if err := respondWithError(w, http.StatusBadRequest, "Password must be at least 5 characters long"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}
	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error hashing password: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	dbUser, err := cfg.dbQueries.CreateUser(r.Context(), database.CreateUserParams{
		Email:          req.Email,
		HashedPassword: hashedPassword,
	})
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error creating user: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	user := User{
		ID:          dbUser.ID,
		CreatedAt:   dbUser.CreatedAt,
		UpdatedAt:   dbUser.UpdatedAt,
		Email:       dbUser.Email,
		IsChirpyRed: dbUser.IsChirpyRed,
	}

	if err := respondWithJSON(w, http.StatusCreated, user); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) updateUser(w http.ResponseWriter, r *http.Request) {
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		if err := respondWithError(w, http.StatusUnauthorized, "Invalid or expired token"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	type request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	var hashedPassword string
	if req.Password != "" {
		if len(req.Password) < 5 {
			if err := respondWithError(w, http.StatusBadRequest, "Password must be at least 5 characters long"); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
		hashedPassword, err = auth.HashPassword(req.Password)
		if err != nil {
			if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error hashing password: %v", err)); err != nil {
				log.Printf("Error responding with error: %v", err)
			}
			return
		}
	}

	dbUser, err := cfg.dbQueries.UpdateUser(r.Context(), database.UpdateUserParams{
		ID:             userID,
		Email:          req.Email,
		HashedPassword: hashedPassword,
	})
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error updating user: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	user := User{
		ID:          dbUser.ID,
		CreatedAt:   dbUser.CreatedAt,
		UpdatedAt:   dbUser.UpdatedAt,
		Email:       dbUser.Email,
		IsChirpyRed: dbUser.IsChirpyRed,
	}

	if err := respondWithJSON(w, http.StatusOK, user); err != nil {
		log.Printf("Error responding with JSON: %v", err)
	}
}

func (cfg *apiConfig) polkaWebhooks(w http.ResponseWriter, r *http.Request) {
	apiKey, err := auth.GetAPIKey(r.Header)
	if err != nil || apiKey != cfg.polkaKey {
		if err := respondWithError(w, http.StatusUnauthorized, "Missing or invalid API key"); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	type request struct {
		Event string `json:"event"`
		Data  struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}

	decoder := json.NewDecoder(r.Body)
	req := request{}
	if err := decoder.Decode(&req); err != nil {
		if err := respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	if req.Event != "user.upgraded" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	userID, err := uuid.Parse(req.Data.UserID)
	if err != nil {
		if err := respondWithError(w, http.StatusNotFound, fmt.Sprintf("Unknown user ID: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	_, err = cfg.dbQueries.UpdateUserChirpyRedStatus(r.Context(), database.UpdateUserChirpyRedStatusParams{
		ID:          userID,
		IsChirpyRed: true,
	})
	if err != nil {
		if err := respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Error upgrading user: %v", err)); err != nil {
			log.Printf("Error responding with error: %v", err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
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
