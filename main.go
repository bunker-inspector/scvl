package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/context"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/joho/godotenv"
	"github.com/mssola/user_agent"
	qrcode "github.com/skip2/go-qrcode"
	"github.com/tomasen/realip"
)

// build flags
var (
	revision string
)

var client *redisClient
var store *sessions.CookieStore

func main() {
	rand.Seed(time.Now().UTC().UnixNano())
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	store = sessions.NewCookieStore([]byte(os.Getenv("SESSION_SECRET")))

	client, err = newRedisClient()
	if err != nil {
		log.Fatalf("Failed to create redisClient: %v", err)
	}
	defer client.Close()
	setupRoutes()
	setupGoogleConfig()
	setupManager()
	defer manager.db.Close()
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func setupRoutes() {
	r := mux.NewRouter()

	r.Handle("/shorten", authenticate(shortenHandler)).Methods(http.MethodPost)
	r.Handle("/", authenticate(rootHandler)).Methods(http.MethodGet)

	r.HandleFunc("/{slug}/qr.png", qrHandler).Methods(http.MethodGet)
	r.Handle("/{slug}/edit", authenticate(editHandler)).Methods(http.MethodGet)
	r.HandleFunc("/{slug}", redirectHandler).Methods(http.MethodGet)
	r.HandleFunc("/{slug}", authenticate(updateHandler)).Methods(http.MethodPost, http.MethodPut, http.MethodPatch)
	r.HandleFunc("/oauth/google/callback", oauthCallbackHandler).Methods(http.MethodGet)
	http.Handle("/css/", http.StripPrefix("/css/", http.FileServer(http.Dir("css/"))))
	http.Handle("/js/", http.StripPrefix("/js/", http.FileServer(http.Dir("js/"))))
	http.Handle("/", r)
}

func authenticate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "scvl")
		userID, ok := session.Values["user_id"].(uint)
		if ok {
			user, err := manager.findUser(userID)
			if err != nil {
				ok = false
			} else {
				context.Set(r, "user", &user)
			}
		}
		if !ok {
			state := generateSlug() + generateSlug()
			session.Values["google_state"] = state
			session.Save(r, w)
			context.Set(r, "login_url", googleConfig.AuthCodeURL(state))
		}
		h.ServeHTTP(w, r)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	bytes, _ := getFlash(w, r, "url_slug")
	resp := map[string]interface{}{}
	if bytes != nil {
		json.Unmarshal(bytes, &resp)
	}
	user, ok := context.Get(r, "user").(*User)
	if ok {
		manager.setPagesToUser(user)
		resp["User"] = user
	}
	loginURL, ok := context.Get(r, "login_url").(string)
	if ok {
		resp["LoginURL"] = loginURL
	}
	renderTemplate(w, r, "/index.tpl", resp)
}

func oauthCallbackHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "scvl")
	retrievedState, _ := session.Values["google_state"].(string)
	if retrievedState != r.URL.Query().Get("state") {
		http.Error(w, fmt.Sprintf("Invalid session state: %s", retrievedState), http.StatusUnauthorized)
		return
	}
	u, err := fetchUserInfo(r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	allowedDomain := os.Getenv("ALLOWED_DOMAIN")
	if allowedDomain != "" && !strings.HasSuffix(u.Email, "@"+allowedDomain) {
		http.Error(w, "ログインは、Scovilleアカウントである必要があります", http.StatusUnprocessableEntity)
		return
	}
	user, err := manager.findOrCreateUser(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	session.Values["user_id"] = user.ID
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func shortenHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := context.Get(r, "user").(*User)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	url := r.FormValue("url")
	if url == "" {
		http.Error(w, "url cannot be empty", http.StatusUnprocessableEntity)
		return
	}

	slug := generateSlug()
	page, err := manager.createPage(user.ID, slug, url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.FormValue("ogp") == "on" {
		ogp := OGP{
			PageID:      int(page.ID),
			Description: r.FormValue("description"),
			Image:       r.FormValue("image"),
			Title:       r.FormValue("title"),
		}
		err = manager.createOGP(&ogp)
		client.SetOGPID(page.Slug, int(ogp.ID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	client.SetURL(slug, url)
	bytes, _ := json.Marshal(map[string]string{
		"URL":  url,
		"Slug": slug,
	})
	setFlash(w, "url_slug", bytes)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func redirectHandler(w http.ResponseWriter, r *http.Request) {
	slug := mux.Vars(r)["slug"]
	url := client.GetURL(slug)
	var ogp *OGP
	if url == "" {
		// Redisでページが見つからなかった場合の処理
		page, err := manager.findPageBySlug(slug)
		if err != nil {
			http.Error(w, "The URL you are looking for is not found.", http.StatusNotFound)
			return
		}
		url = page.URL
		client.SetURL(slug, url)
		if page.OGP != nil {
			ogp = page.OGP
			client.SetOGPID(slug, int(page.OGP.ID))
		}
	}
	ua := user_agent.New(r.UserAgent())
	if !ua.Bot() {
		name, _ := ua.Browser()
		manager.createPageView(slug, PageView{
			RealIP:      realip.RealIP(r),
			Referer:     r.Referer(),
			Mobile:      ua.Mobile(),
			Platform:    ua.Platform(),
			OS:          ua.OS(),
			BrowserName: name,
		})
	}
	var ogpID int
	if ogp == nil {
		ogpID = client.GetOGPID(slug)
	}
	if ogpID != 0 {
		if ogp == nil {
			ogp, _ = manager.findOGPByID(ogpID)
		}
		if ogp != nil {
			data := map[string]interface{}{
				"URL": url,
				"OGP": ogp,
			}
			tpl := findTemplateWithoutBase("/redirect.tpl")
			tpl.Execute(w, data)
			return
		}
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func qrHandler(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(strings.Split(r.RequestURI, "/qr.png")[0], qrcode.Medium, 256)
	if err != nil {
		log.Println("Failed to generate QR code: ", err)
		http.Error(w, "Failed to generate QR code", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	if _, err := w.Write(png); err != nil {
		log.Println("Unable to write image: ", err)
		http.Error(w, "Unable to write image", http.StatusInternalServerError)
	}
}

func editHandler(w http.ResponseWriter, r *http.Request) {
	bytes, _ := getFlash(w, r, "message")
	resp := map[string]interface{}{}
	if bytes != nil {
		json.Unmarshal(bytes, &resp)
	}

	user, ok := context.Get(r, "user").(*User)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	slug := mux.Vars(r)["slug"]
	page, err := manager.findPageBySlug(slug)
	if err != nil {
		http.Error(w, "The page you are looking for is not found.", http.StatusNotFound)
		return
	}

	if page.UserID != int(user.ID) {
		http.Error(w, "You don't have permission to edit it.", http.StatusUnauthorized)
		return
	}

	resp["Page"] = page
	if page.OGP != nil {
		resp["OGP"] = true
	}
	renderTemplate(w, r, "/edit.tpl", resp)
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := context.Get(r, "user").(*User)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	slug := mux.Vars(r)["slug"]
	page, err := manager.findPageBySlug(slug)
	if err != nil {
		http.Error(w, "The page you are looking for is not found.", http.StatusNotFound)
		return
	}

	if page.UserID != int(user.ID) {
		http.Error(w, "You don't have permission to edit it.", http.StatusUnauthorized)
		return
	}

	url := r.FormValue("url")
	if url == "" {
		http.Error(w, "url cannot be empty", http.StatusUnprocessableEntity)
		return
	}
	if err := manager.updatePage(page.ID, url); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	client.SetURL(slug, url)
	if r.FormValue("ogp") == "on" {
		var ogpID int
		if page.OGP == nil {
			ogp := OGP{
				PageID:      int(page.ID),
				Description: r.FormValue("description"),
				Image:       r.FormValue("image"),
				Title:       r.FormValue("title"),
			}
			err = manager.createOGP(&ogp)
			ogpID = int(ogp.ID)
		} else {
			err = manager.updateOGP(page.OGP.ID, OGP{
				Description: r.FormValue("description"),
				Image:       r.FormValue("image"),
				Title:       r.FormValue("title"),
			})
			ogpID = int(page.OGP.ID)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		client.SetOGPID(page.Slug, ogpID)
	} else if page.OGP != nil {
		client.DeleteOGPID(page.Slug)
		err = manager.deleteOGP(page.OGP.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	bytes, _ := json.Marshal(map[string]string{
		"Success": "Update succeeded.",
	})
	setFlash(w, "message", bytes)
	http.Redirect(w, r, "/"+slug+"/edit", http.StatusSeeOther)
}
