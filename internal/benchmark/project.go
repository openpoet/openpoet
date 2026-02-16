package benchmark

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TestProject holds a simulated project structure in memory.
type TestProject struct {
	files map[string]string
	tasks []testTask
}

type testTask struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	ParentID    *int   `json:"parent_id,omitempty"`
}

// NewTestProject creates a test project with realistic Go webapp files and tasks.
func NewTestProject() *TestProject {
	p := &TestProject{
		files: make(map[string]string),
	}
	p.initFiles()
	p.initTasks()
	return p
}

func (p *TestProject) initFiles() {
	p.files["CLAUDE.md"] = `# Test Webapp

## Overview
A simple Go REST API for managing users.

## Tech Stack
- Go 1.23, chi router, SQLite

## Status
- Users CRUD: done
- Health check: done
- Authentication: not started
- Deploy pipeline: not started

## Guidelines
- Use structured logging
- All handlers must return JSON
- Tests required for new features
`

	p.files["README.md"] = `# test-webapp

A simple Go REST API for user management.

## Running
` + "```" + `bash
go run main.go
` + "```" + `

Server starts on :8080.

## Endpoints
- GET /health — health check
- GET /users — list users
- POST /users — create user
- GET /users/{id} — get user
- PUT /users/{id} — update user
- DELETE /users/{id} — delete user
`

	p.files["go.mod"] = `module test-webapp

go 1.23

require github.com/go-chi/chi/v5 v5.0.12
`

	p.files["main.go"] = `package main

import (
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"test-webapp/handlers"
)

func main() {
	r := chi.NewRouter()

	r.Get("/health", handlers.HealthCheck)
	r.Route("/users", func(r chi.Router) {
		r.Get("/", handlers.ListUsers)
		r.Post("/", handlers.CreateUser)
		r.Get("/{id}", handlers.GetUser)
		r.Put("/{id}", handlers.UpdateUser)
		r.Delete("/{id}", handlers.DeleteUser)
	})

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}
`

	p.files["handlers/health.go"] = `package handlers

import (
	"encoding/json"
	"net/http"
)

func HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"version": "1.0.0",
	})
}
`

	p.files["handlers/users.go"] = `package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"test-webapp/models"
)

var users = []models.User{
	{ID: 1, Name: "Alice", Email: "alice@example.com"},
	{ID: 2, Name: "Bob", Email: "bob@example.com"},
}

func ListUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(users)
}

func CreateUser(w http.ResponseWriter, r *http.Request) {
	var u models.User
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u.ID = len(users) + 1
	users = append(users, u)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(u)
}

func GetUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	for _, u := range users {
		if u.ID == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(u)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func UpdateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	var update models.User
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for i, u := range users {
		if u.ID == id {
			users[i].Name = update.Name
			users[i].Email = update.Email
			json.NewEncoder(w).Encode(users[i])
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func DeleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))
	for i, u := range users {
		if u.ID == id {
			users = append(users[:i], users[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}
`

	p.files["models/user.go"] = `package models

type User struct {
	ID    int    ` + "`json:\"id\"`" + `
	Name  string ` + "`json:\"name\"`" + `
	Email string ` + "`json:\"email\"`" + `
}
`
}

func (p *TestProject) initTasks() {
	p.tasks = []testTask{
		{ID: 1, Title: "Implementar autenticação JWT", Description: "Adicionar auth JWT nos endpoints. Critérios: login retorna token, endpoints protegidos requerem token válido.", Status: "todo", Priority: "high"},
		{ID: 2, Title: "Configurar pipeline de deploy", Description: "Criar pipeline CI/CD para deploy automático. Critérios: push na main dispara deploy, rollback funciona.", Status: "todo", Priority: "medium"},
		{ID: 3, Title: "Adicionar testes unitários", Description: "Cobertura de testes para handlers e models. Critérios: cobertura > 80%, testes passam no CI.", Status: "in_progress", Priority: "high"},
		{ID: 4, Title: "Corrigir validação de email", Description: "Email aceita formato inválido. Critérios: emails inválidos retornam 400, emails válidos são aceitos.", Status: "todo", Priority: "urgent"},
		{ID: 5, Title: "Documentar API com OpenAPI", Description: "Gerar spec OpenAPI 3.0 dos endpoints. Critérios: spec válida, Swagger UI disponível em /docs.", Status: "done", Priority: "low"},
	}
}

// Files returns all project files.
func (p *TestProject) Files() map[string]string {
	return p.files
}

// MemoryDoc returns the CLAUDE.md content.
func (p *TestProject) MemoryDoc() string {
	return p.files["CLAUDE.md"]
}

// Skills returns mock skill names for the system prompt.
func (p *TestProject) Skills() []string {
	return []string{
		"go-best-practices (coding) - enabled",
		"api-testing (testing) - enabled",
	}
}

// Projects returns mock project names for the system prompt.
func (p *TestProject) Projects() []string {
	return []string{
		"[ID:1] test-webapp (/projects/test-webapp) - local",
	}
}

// TasksJSON returns tasks as JSON (for list_tasks mock response).
func (p *TestProject) TasksJSON() string {
	b, _ := json.MarshalIndent(p.tasks, "", "  ")
	return string(b)
}

// ProjectsJSON returns projects as JSON (for list_projects mock response).
func (p *TestProject) ProjectsJSON() string {
	return `[{"id": 1, "name": "test-webapp", "path": "/projects/test-webapp", "type": "local"}]`
}

// DirectoryJSON returns a directory listing (for list_directory mock response).
func (p *TestProject) DirectoryJSON(path any) string {
	prefix := ""
	if s, ok := path.(string); ok && s != "" {
		prefix = s
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
	}

	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int    `json:"size"`
	}

	seen := make(map[string]bool)
	var entries []entry

	for filePath, content := range p.files {
		if prefix != "" && !strings.HasPrefix(filePath, prefix) {
			continue
		}
		relative := strings.TrimPrefix(filePath, prefix)
		parts := strings.SplitN(relative, "/", 2)
		name := parts[0]

		if seen[name] {
			continue
		}
		seen[name] = true

		if len(parts) > 1 {
			entries = append(entries, entry{Name: name, Type: "directory", Size: 0})
		} else {
			entries = append(entries, entry{Name: name, Type: "file", Size: len(content)})
		}
	}

	b, _ := json.MarshalIndent(entries, "", "  ")
	return string(b)
}

// FileContent returns the content of a file (for read_file mock response).
func (p *TestProject) FileContent(path any) string {
	s, ok := path.(string)
	if !ok {
		return `{"error": "invalid path"}`
	}
	content, exists := p.files[s]
	if !exists {
		return fmt.Sprintf(`{"error": "file not found: %s"}`, s)
	}
	return content
}

// MemoryDocResponse returns the mock get_memory_doc response.
func (p *TestProject) MemoryDocResponse() string {
	return fmt.Sprintf(`{"viewer_url": "/documents/view/memory-1", "content": "<internal_reference>\n%s\n</internal_reference>"}`, p.MemoryDoc())
}

// GrepContentResponse returns mock grep results for a pattern.
func (p *TestProject) GrepContentResponse(pattern any) string {
	pat, ok := pattern.(string)
	if !ok {
		return `[]`
	}
	type match struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	var matches []match
	for filePath, content := range p.files {
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(strings.ToLower(line), strings.ToLower(pat)) {
				matches = append(matches, match{File: filePath, Line: i + 1, Text: line})
			}
		}
	}
	b, _ := json.MarshalIndent(matches, "", "  ")
	return string(b)
}
