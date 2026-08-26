package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/rs/cors"
	"github.com/syumai/workers"
	"github.com/syumai/workers/cloudflare"
	"github.com/syumai/workers/cloudflare/kv"
)

const (
	kvNamespace     = "PINGO_AUTH"
	vapidPublicKey  = "BEJ45uzzL_hw2MpJaxTw8Jwk-hbqJE3D5GI7TWMBaYOLkKoVsJQJGVZrpDOASMBpsCpF3bFI2LFZaZecqAWfAKk"
	vapidPrivateKey = "uwHBVq5KHmevNRtVBTP62G_n6mFeH7vOQsVlpG_YHWU"
	subscriberEmail = "admin@accreativos.com"
)

func generateAuthToken(salt string) string {
	if salt == "" {
		return "public"
	}
	now := time.Now().UTC()
	timeKey := fmt.Sprintf("%d-%d-%d-%d", now.Year(), int(now.Month())-1, now.Day(), now.Hour())
	message := salt + timeKey
	hash := sha256.Sum256([]byte(message))
	return fmt.Sprintf("%x", hash)
}

func checkAuth(req *http.Request, userPubKey string) (bool, string) {
	pingoKV, err := kv.NewNamespace(kvNamespace)
	if err != nil {
		return false, "KV Init Error"
	}
	userDataStr, err := pingoKV.GetString(userPubKey, nil)
	if err != nil || userDataStr == "" {
		return false, "User not authorized"
	}
	var userData struct {
		Salt string `json:"salt"`
	}
	json.Unmarshal([]byte(userDataStr), &userData)
	clientToken := req.Header.Get("X-Pingo-Auth")
	if clientToken == "" {
		return false, "Missing Auth Token"
	}
	if clientToken == generateAuthToken(userData.Salt) {
		return true, ""
	}
	return false, "Invalid Auth Token"
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				fmt.Fprintf(os.Stderr, "[PANIC] %v\n", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Pingo Cloud is active"))
	})

	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			UserPubKey string `json:"userPublicKey"`
			Salt       string `json:"salt"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid Payload", http.StatusBadRequest)
			return
		}
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		data, _ := json.Marshal(payload)
		pingoKV.PutString(payload.UserPubKey, string(data), nil)
		fmt.Fprintf(os.Stderr, "[Auth] Registered: %s\n", payload.UserPubKey)
		w.Write([]byte("User registered"))
	})

	mux.HandleFunc("/turn-credentials", func(w http.ResponseWriter, req *http.Request) {
		userPubKey := req.URL.Query().Get("peerId")
		if ok, msg := checkAuth(req, userPubKey); !ok {
			http.Error(w, msg, http.StatusUnauthorized)
			return
		}
		turnURL := cloudflare.Getenv("TURN_URL")
		turnUser := cloudflare.Getenv("TURN_USERNAME")
		turnCred := cloudflare.Getenv("TURN_CREDENTIAL")
		turnSecret := cloudflare.Getenv("TURN_STATIC_AUTH_SECRET")

		if turnUser != "" && turnCred != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"iceServers": []map[string]any{
					{"urls": []string{turnURL}, "username": turnUser, "credential": turnCred},
				},
			})
			return
		}

		if turnSecret != "" {
			timestamp := time.Now().Unix() + 3600
			username := fmt.Sprintf("%d:%s", timestamp, userPubKey)
			mac := hmac.New(sha1.New, []byte(turnSecret))
			mac.Write([]byte(username))
			password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
			json.NewEncoder(w).Encode(map[string]any{
				"iceServers": []map[string]any{
					{"urls": []string{turnURL}, "username": username, "credential": password},
				},
			})
			return
		}
		http.Error(w, fmt.Sprintf("TURN config missing (URL:%s, User:%s, CredLen:%d)", turnURL, turnUser, len(turnCred)), http.StatusInternalServerError)
	})

	mux.HandleFunc("/push/subscribe", func(w http.ResponseWriter, req *http.Request) {
		var subscription map[string]any
		if err := json.NewDecoder(req.Body).Decode(&subscription); err != nil {
			http.Error(w, "Invalid Payload", http.StatusBadRequest)
			return
		}
		userPubKey, _ := subscription["userPublicKey"].(string)
		salt, _ := subscription["salt"].(string)
		if userPubKey == "" {
			http.Error(w, "UserPubKey required", http.StatusBadRequest)
			return
		}
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subscriptionStr, _ := json.Marshal(subscription)
		pingoKV.PutString("push:"+userPubKey, string(subscriptionStr), nil)
		if salt != "" {
			authPayload := map[string]string{"userPublicKey": userPubKey, "salt": salt}
			authData, _ := json.Marshal(authPayload)
			pingoKV.PutString(userPubKey, string(authData), nil)
		}
		fmt.Fprintf(os.Stderr, "[Push] Subscribed: %s\n", userPubKey)
		w.Write([]byte("Cloud activated"))
	})

	mux.HandleFunc("/push/send/", func(w http.ResponseWriter, req *http.Request) {
		targetId := strings.TrimPrefix(req.URL.Path, "/push/send/")
		senderId := req.URL.Query().Get("from")
		fmt.Fprintf(os.Stderr, "[Push] Relay attempt from %s to %s\n", senderId, targetId)

		if ok, msg := checkAuth(req, senderId); !ok {
			fmt.Fprintf(os.Stderr, "[Push] Auth failed for %s: %s\n", senderId, msg)
			http.Error(w, msg, http.StatusUnauthorized)
			return
		}

		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subStr, err := pingoKV.GetString("push:"+targetId, nil)
		if err != nil || subStr == "" {
			fmt.Fprintf(os.Stderr, "[Push] Target %s not found in KV\n", targetId)
			http.Error(w, "Target offline or not subscribed", http.StatusNotFound)
			return
		}

		s := &webpush.Subscription{}
		// Clean up common non-json values that might be in KV
		if subStr == "null" || subStr == "<null>" || subStr == "" {
			fmt.Fprintf(os.Stderr, "[Push] Target %s has invalid/null subscription in KV\n", targetId)
			http.Error(w, "Target has no valid subscription (try re-subscribing)", http.StatusNotFound)
			return
		}

		if err := json.Unmarshal([]byte(subStr), s); err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Error parsing subscription for %s: %v. Data: [%s]\n", targetId, err, subStr)
			http.Error(w, fmt.Sprintf("Invalid target subscription data: %v (Raw: %s)", err, subStr), http.StatusInternalServerError)
			return
		}

		// Prepare Payload from request body
		var pushPayload map[string]any
		json.NewDecoder(req.Body).Decode(&pushPayload)
		payloadBytes, _ := json.Marshal(pushPayload)

		fmt.Fprintf(os.Stderr, "[Push] Sending to endpoint: %s\n", s.Endpoint)
		resp, err := webpush.SendNotification(payloadBytes, s, &webpush.Options{
			HTTPClient:      http.DefaultClient,
			Subscriber:      subscriberEmail,
			VAPIDPublicKey:  vapidPublicKey,
			VAPIDPrivateKey: vapidPrivateKey,
			TTL:             30,
		})

		if err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Error from SendNotification: %v\n", err)
			http.Error(w, fmt.Sprintf("Push delivery failed: %v", err), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		w.WriteHeader(resp.StatusCode)
		w.Write([]byte("Push sent"))
	})

	// Test push endpoint (no auth)
	mux.HandleFunc("/push/test/{peerId}", func(w http.ResponseWriter, req *http.Request) {
		targetId := req.PathValue("peerId")
		pingoKV, _ := kv.NewNamespace(kvNamespace)
		subStr, _ := pingoKV.GetString("push:"+targetId, nil)
		if subStr == "" {
			http.Error(w, "Target not registered", 404)
			return
		}

		var s webpush.Subscription
		json.Unmarshal([]byte(subStr), &s)

		payloadBytes, _ := json.Marshal(map[string]string{
			"title": "Prueba de Pingo",
			"body":  "Si ves esto, la entrega funciona a pesar de la seguridad.",
			"url":   "/",
		})

		resp, err := webpush.SendNotification(payloadBytes, &s, &webpush.Options{
			HTTPClient:      http.DefaultClient,
			Subscriber:      subscriberEmail,
			VAPIDPublicKey:  vapidPublicKey,
			VAPIDPrivateKey: vapidPrivateKey,
			TTL:             30,
		})

		if err != nil {
			fmt.Fprintf(os.Stderr, "[Push] Test Error: %v\n", err)
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "[Push] FCM Response (%d): %s\n", resp.StatusCode, string(respBody))

		w.WriteHeader(resp.StatusCode)
		w.Write([]byte("Push sent"))
	})

	// Git CORS Proxy
	mux.HandleFunc("/git-proxy/", func(w http.ResponseWriter, req *http.Request) {
		targetPath := strings.TrimPrefix(req.URL.Path, "/git-proxy/")
		if targetPath == "" {
			http.Error(w, "Target required", http.StatusBadRequest)
			return
		}

		// Determine target URL - assume https if not provided
		targetURL := targetPath
		if !strings.HasPrefix(targetURL, "http") {
			targetURL = "https://" + targetURL
		}

		if req.URL.RawQuery != "" {
			targetURL += "?" + req.URL.RawQuery
		}

		fmt.Fprintf(os.Stderr, "[GitProxy] Method: %s, Forwarding to: %s\n", req.Method, targetURL)

		proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
		if err != nil {
			http.Error(w, "Request Creation Error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Copy relevant headers from client
		for name, values := range req.Header {
			normalizedName := strings.ToLower(name)
			// Skip headers that should be handled by the proxy or are internal
			if normalizedName == "host" || normalizedName == "x-pingo-auth" ||
				normalizedName == "cf-ray" || normalizedName == "cf-connecting-ip" ||
				normalizedName == "x-real-ip" || normalizedName == "x-forwarded-for" {
				continue
			}
			for _, value := range values {
				proxyReq.Header.Add(name, value)
			}
		}

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[GitProxy] Gateway Error: %v\n", err)
			http.Error(w, "Gateway Error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy response headers back to client
		for name, values := range resp.Header {
			// Some headers shouldn't be copied back or will be overridden by CORS middleware
			normalizedName := strings.ToLower(name)
			if normalizedName == "access-control-allow-origin" || normalizedName == "access-control-allow-credentials" {
				continue
			}
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})

	// Setup CORS and Middleware
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "X-Pingo-Auth"},
	})
	handler := recoverMiddleware(c.Handler(mux))

	workers.Serve(handler)
}
