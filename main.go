package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Name struct {
	ID   primitive.ObjectID `json:"id,omitempty" bson:"_id,omitempty"`
	Name string             `json:"name" bson:"name"`
}

var (
	client     *mongo.Client
	collection *mongo.Collection
)

func main() {
	// ---- Mongo init ----
	mongoURI := getenv("MONGO_URI", "mongodb://localhost:27017")
	dbName := getenv("DB_NAME", "testdb")
	colName := getenv("COLLECTION", "names")

	var err error
	client, err = mongo.Connect(context.Background(), options.Client().ApplyURI(mongoURI))
	must(err)
	must(client.Ping(context.Background(), nil))

	collection = client.Database(dbName).Collection(colName)
	log.Printf("Connected to MongoDB %s, DB=%s, Collection=%s", mongoURI, dbName, colName)

	// ---- HTTP routes ----
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/names", namesHandler)     // POST /names, GET /names
	http.HandleFunc("/names/", nameByIDHandler) // GET/PUT/DELETE /names/{id}

	addr := getenv("ADDR", ":8080")
	log.Printf("Serving on %s", addr)
	must(http.ListenAndServe(addr, corsMiddleware(http.DefaultServeMux)))
}

// ========== Handlers ==========

func healthHandler(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]string{"status": "ok"})
}

// POST /names  { "name": "Alice" }
// GET  /names  -> list
func namesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var payload Name
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			badRequest(w, "invalid JSON: "+err.Error()); return
		}
		payload.Name = strings.TrimSpace(payload.Name)
		if payload.Name == "" {
			badRequest(w, "`name` is required"); return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := collection.InsertOne(ctx, bson.M{"name": payload.Name})
		if err != nil {
			internal(w, err); return
		}
		id := res.InsertedID.(primitive.ObjectID)
		created(w, Name{ID: id, Name: payload.Name})

	case http.MethodGet:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cur, err := collection.Find(ctx, bson.D{})
		if err != nil {
			internal(w, err); return
		}
		defer cur.Close(ctx)

		var out []Name
		for cur.Next(ctx) {
			var n Name
			if err := cur.Decode(&n); err != nil { internal(w, err); return }
			out = append(out, n)
		}
		if err := cur.Err(); err != nil {
			internal(w, err); return
		}
		ok(w, out)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// GET /names/{id}
// PUT /names/{id}  { "name": "Bob" }
// DELETE /names/{id}
func nameByIDHandler(w http.ResponseWriter, r *http.Request) {
	idStr, err := extractID(r.URL.Path, "/names/")
	if err != nil { notFound(w); return }

	oid, err := primitive.ObjectIDFromHex(idStr)
	if err != nil { badRequest(w, "invalid id"); return }

	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var n Name
		err := collection.FindOne(ctx, bson.M{"_id": oid}).Decode(&n)
		if errors.Is(err, mongo.ErrNoDocuments) { notFound(w); return }
		if err != nil { internal(w, err); return }
		ok(w, n)

	case http.MethodPut:
		var payload struct{ Name string `json:"name"` }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			badRequest(w, "invalid JSON: "+err.Error()); return
		}
		payload.Name = strings.TrimSpace(payload.Name)
		if payload.Name == "" {
			badRequest(w, "`name` is required"); return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := collection.UpdateByID(ctx, oid, bson.M{"$set": bson.M{"name": payload.Name}})
		if err != nil { internal(w, err); return }
		if res.MatchedCount == 0 { notFound(w); return }
		ok(w, Name{ID: oid, Name: payload.Name})

	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := collection.DeleteOne(ctx, bson.M{"_id": oid})
		if err != nil { internal(w, err); return }
		if res.DeletedCount == 0 { notFound(w); return }
		noContent(w)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// ========== Helpers ==========

func extractID(path, prefix string) (string, error) {
	if !strings.HasPrefix(path, prefix) { return "", errors.New("bad path") }
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 1 || parts[0] == "" { return "", errors.New("no id") }
	return parts[0], nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}

func must(err error) {
	if err != nil { log.Fatal(err) }
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if r.Method == http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
		next.ServeHTTP(w, r)
	})
}

// ---- response helpers ----
func jsonWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func ok(w http.ResponseWriter, v any)          { jsonWrite(w, http.StatusOK, v) }
func created(w http.ResponseWriter, v any)     { jsonWrite(w, http.StatusCreated, v) }
func badRequest(w http.ResponseWriter, msg any){ jsonWrite(w, http.StatusBadRequest, map[string]any{"error": msg}) }
func notFound(w http.ResponseWriter)           { jsonWrite(w, http.StatusNotFound, map[string]string{"error":"not found"}) }
func internal(w http.ResponseWriter, err error){ jsonWrite(w, http.StatusInternalServerError, map[string]any{"error": err.Error()}) }
func noContent(w http.ResponseWriter)          { w.WriteHeader(http.StatusNoContent) }
func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	jsonWrite(w, http.StatusMethodNotAllowed, map[string]any{"error":"method not allowed","allow":allowed})
}
