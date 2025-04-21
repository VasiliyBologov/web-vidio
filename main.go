package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"

	//"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/gorilla/websocket"
	//"github.com/lib/pq"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Структуры данных
type Client struct {
	conn     *websocket.Conn
	partner  *Client
	mutex    sync.Mutex
	isBusy   bool
	userID   int
	username string
	reports  []string
}

type Message struct {
	Type     string      `json:"type"`
	Message  string      `json:"message,omitempty"`
	Username string      `json:"username,omitempty"`
	UserID   int         `json:"userId,omitempty"`
	Data     interface{} `json:"data,omitempty"`
}

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	IsOnline bool   `json:"isOnline"`
}

var (
	waitingClients = make(map[int]*Client)
	onlineClients  = make(map[int]*Client)
	waitMutex      sync.Mutex
	db             *sql.DB
	jwtSecret      = []byte("your-secret-key") // В продакшене использовать безопасный ключ
)

func initDB(config *Config) (*sql.DB, error) {
	db, err := sql.Open("postgres", config.DatabaseURL)
	if err != nil {
		return nil, err
	}

	// Проверка соединения
	err = db.Ping()
	if err != nil {
		return nil, err
	}

	// Создание таблиц
	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS users (
            id SERIAL PRIMARY KEY,
            username VARCHAR(255) UNIQUE NOT NULL,
            last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );

        CREATE TABLE IF NOT EXISTS friends (
            user_id INT REFERENCES users(id),
            friend_id INT REFERENCES users(id),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            PRIMARY KEY (user_id, friend_id)
        );

        CREATE TABLE IF NOT EXISTS reports (
            id SERIAL PRIMARY KEY,
            reporter_id INT REFERENCES users(id),
            reported_id INT REFERENCES users(id),
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        );
    `)

	return db, err
}

type Config struct {
	DatabaseURL string
	JWTSecret   string
}

func loadConfig() *Config {
	dbHost := os.Getenv("DB_HOST")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dbPort := os.Getenv("DB_PORT")

	dbURL := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		dbUser, dbPassword, dbHost, dbPort, dbName,
	)

	return &Config{
		DatabaseURL: dbURL,
		JWTSecret:   os.Getenv("JWT_SECRET"),
	}
}

func main() {
	config := loadConfig()

	var err error
	db, err = initDB(config)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	jwtSecret = []byte(config.JWTSecret)

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/friends", handleFriends)

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var data struct {
		Username string `json:"username"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var userID int
	err := db.QueryRow(`
        INSERT INTO users (username)
        VALUES ($1)
        ON CONFLICT (username) DO UPDATE
        SET last_seen = CURRENT_TIMESTAMP
        RETURNING id
    `, data.Username).Scan(&userID)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Создаем JWT токен
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  userID,
		"username": data.Username,
		"exp":      time.Now().Add(time.Hour * 24 * 7).Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":    tokenString,
		"user_id":  userID,
		"username": data.Username,
	})
}

func handleFriends(w http.ResponseWriter, r *http.Request) {
	// Проверка токена
	token := r.Header.Get("Authorization")
	claims, err := validateToken(token)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	userID := int(claims["user_id"].(float64))

	switch r.Method {
	case http.MethodGet:
		// Получение списка друзей
		rows, err := db.Query(`
            SELECT u.id, u.username, 
                   CASE WHEN u.last_seen > NOW() - INTERVAL '5 minutes' 
                        THEN true ELSE false END as is_online
            FROM friends f
            JOIN users u ON f.friend_id = u.id
            WHERE f.user_id = $1
        `, userID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var friends []User
		for rows.Next() {
			var friend User
			err := rows.Scan(&friend.ID, &friend.Username, &friend.IsOnline)
			if err != nil {
				continue
			}
			friends = append(friends, friend)
		}

		json.NewEncoder(w).Encode(friends)

	case http.MethodPost:
		// Добавление друга
		var data struct {
			FriendID int `json:"friendId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		_, err := db.Exec(`
            INSERT INTO friends (user_id, friend_id)
            VALUES ($1, $2)
            ON CONFLICT DO NOTHING
        `, userID, data.FriendID)

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}
}

func validateToken(tokenString string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, err
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Проверка токена из URL параметра
	token := r.URL.Query().Get("token")
	claims, err := validateToken(token)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	userID := int(claims["user_id"].(float64))
	username := claims["username"].(string)

	client := &Client{
		conn:     conn,
		isBusy:   false,
		userID:   userID,
		username: username,
	}

	// Обновляем статус онлайн
	db.Exec("UPDATE users SET last_seen = CURRENT_TIMESTAMP WHERE id = $1", userID)

	// Добавляем клиента в карту онлайн пользователей
	onlineClients[userID] = client

	// Обработка сообщений
	go handleMessages(client)
}

func handleMessages(client *Client) {
	defer func() {
		disconnectClient(client)
		client.conn.Close()
		delete(onlineClients, client.userID)
		db.Exec("UPDATE users SET last_seen = CURRENT_TIMESTAMP WHERE id = $1", client.userID)
	}()

	for {
		var msg Message
		err := client.conn.ReadJSON(&msg)
		if err != nil {
			log.Println("Read error:", err)
			break
		}

		switch msg.Type {
		case "findPartner":
			if msg.UserID > 0 { // Ищем конкретного друга
				if partner, ok := onlineClients[msg.UserID]; ok && !partner.isBusy {
					connectPartners(client, partner)
				} else {
					client.conn.WriteJSON(Message{
						Type:    "error",
						Message: "Friend is not available",
					})
				}
			} else { // Ищем случайного партнера
				findRandomPartner(client)
			}

		case "message":
			if client.partner != nil {
				msg.Username = client.username
				msg.UserID = client.userID
				client.partner.conn.WriteJSON(msg)
			}

		case "addFriend":
			if client.partner != nil {
				// Добавляем запись в базу данных
				_, err := db.Exec(`
                    INSERT INTO friends (user_id, friend_id)
                    VALUES ($1, $2)
                    ON CONFLICT DO NOTHING
                `, client.userID, client.partner.userID)

				if err != nil {
					client.conn.WriteJSON(Message{
						Type:    "error",
						Message: "Failed to add friend",
					})
				} else {
					client.conn.WriteJSON(Message{
						Type:     "friendAdded",
						Message:  "Friend added successfully",
						UserID:   client.partner.userID,
						Username: client.partner.username,
					})
				}
			}

		case "skip":
			skipPartner(client)
		}
	}
}

func findRandomPartner(client *Client) {
	waitMutex.Lock()
	defer waitMutex.Unlock()

	// Ищем случайного партнера из ожидающих
	if len(waitingClients) > 0 {
		// Получаем случайного партнера
		var partnerID int
		for id := range waitingClients {
			partnerID = id
			break
		}
		partner := waitingClients[partnerID]
		delete(waitingClients, partnerID)

		connectPartners(client, partner)
	} else {
		waitingClients[client.userID] = client
		client.conn.WriteJSON(Message{
			Type:    "waiting",
			Message: "Waiting for partner...",
		})
	}
}

func connectPartners(client1, client2 *Client) {
	client1.partner = client2
	client2.partner = client1
	client1.isBusy = true
	client2.isBusy = true

	client1.conn.WriteJSON(Message{
		Type:     "connected",
		Message:  "Partner found!",
		Username: client2.username,
		UserID:   client2.userID,
	})

	client2.conn.WriteJSON(Message{
		Type:     "connected",
		Message:  "Partner found!",
		Username: client1.username,
		UserID:   client1.userID,
	})
}

func skipPartner(client *Client) {
	if client.partner != nil {
		partner := client.partner

		// Уведомляем партнера
		partner.conn.WriteJSON(Message{
			Type:    "skipped",
			Message: "Partner skipped you",
		})

		// Разрываем связь
		client.partner = nil
		partner.partner = nil
		client.isBusy = false
		partner.isBusy = false

		// Ищем новых партнеров
		go findRandomPartner(client)
		go findRandomPartner(partner)
	}
}

func disconnectClient(client *Client) {
	waitMutex.Lock()
	defer waitMutex.Unlock()

	delete(waitingClients, client.userID)

	if client.partner != nil {
		client.partner.conn.WriteJSON(Message{
			Type:    "disconnected",
			Message: "Partner disconnected",
		})
		client.partner.partner = nil
		client.partner.isBusy = false
	}
}
