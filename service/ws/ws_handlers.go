package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/KAsare1/Kodefx-server/cmd/models"
	"github.com/KAsare1/Kodefx-server/cmd/utils"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	expo "github.com/oliveroneill/exponent-server-sdk-golang/sdk"
	"gorm.io/gorm"
)

type ChatHandler struct {
	db                 *gorm.DB
	hub                *models.Hub
	notificationSender NotificationSender
}

// NotificationSender interface defines methods for sending notifications
type NotificationSender interface {
	SendUserNotification(userID string, title, body string, data map[string]interface{}) (bool, error)
}

// DefaultNotificationSender implements the NotificationSender interface
type DefaultNotificationSender struct {
	db         *gorm.DB
	expoClient *expo.PushClient
}

// SendUserNotification sends a notification to all devices of a user
func (s *DefaultNotificationSender) SendUserNotification(userID string, title, body string, data map[string]interface{}) (bool, error) {
	// Get user's devices
	var devices []models.Device
	result := s.db.Where("user_id = ?", userID).Find(&devices)

	if result.Error != nil {
		return false, fmt.Errorf("error retrieving user devices: %v", result.Error)
	}

	if len(devices) == 0 {
		return true, nil // No devices to notify, but not an error
	}

	// Collect all tokens for this user
	var tokens []string
	for _, device := range devices {
		tokens = append(tokens, device.Token)
	}

	// Send notification to all user devices using SDK
	success, err := s.sendExpoNotificationSDK(tokens, title, body, data)

	// Create notification history
	status := "sent"
	if !success || err != nil {
		status = "failed"
	}

	// Convert data to JSON string
	dataJSON, _ := json.Marshal(data)

	history := models.NotificationHistory{
		UserID: userID,
		Title:  title,
		Body:   body,
		Data:   string(dataJSON),
		Status: status,
		SentAt: time.Now(),
	}

	if dbErr := s.db.Create(&history).Error; dbErr != nil {
		// Log this error but don't fail the request
		log.Printf("Error creating notification history: %v", dbErr)
	}

	return success, err
}

// sendExpoNotificationSDK sends push notifications using the Expo SDK
func (s *DefaultNotificationSender) sendExpoNotificationSDK(tokenStrings []string, title, body string, data map[string]interface{}) (bool, error) {
	// Convert string tokens to ExponentPushToken
	var pushTokens []expo.ExponentPushToken
	var invalidTokens []string

	for _, tokenString := range tokenStrings {
		pushToken, err := expo.NewExponentPushToken(tokenString)
		if err != nil {
			log.Printf("Invalid push token format %s: %v", tokenString, err)
			invalidTokens = append(invalidTokens, tokenString)
			continue // Skip invalid tokens instead of failing completely
		}
		pushTokens = append(pushTokens, pushToken)
	}

	if len(pushTokens) == 0 {
		return false, fmt.Errorf("no valid push tokens found")
	}

	// Convert data to map[string]string as required by the SDK
	var stringData map[string]string
	if data != nil {
		stringData = make(map[string]string)
		for key, value := range data {
			// Convert all values to strings
			stringData[key] = fmt.Sprintf("%v", value)
		}
	}

	// Create the push message
	pushMessage := &expo.PushMessage{
		To:       pushTokens,
		Body:     body,
		Title:    title,
		Sound:    "default",
		Priority: expo.DefaultPriority,
		Data:     stringData,
	}

	// Send the notification
	response, err := s.expoClient.Publish(pushMessage)
	if err != nil {
		return false, fmt.Errorf("failed to publish notification: %v", err)
	}

	// Check for any validation errors in the response
	if validationErr := response.ValidateResponse(); validationErr != nil {
		log.Printf("Push notification validation error: %v", validationErr)

		// Clean up invalid tokens from database
		s.cleanupInvalidTokens(invalidTokens)

		return false, fmt.Errorf("notification validation failed: %v", validationErr)
	}

	// Clean up any invalid tokens we found during token conversion
	if len(invalidTokens) > 0 {
		s.cleanupInvalidTokens(invalidTokens)
	}

	return true, nil
}

// Helper function to remove invalid tokens from database
func (s *DefaultNotificationSender) cleanupInvalidTokens(tokens []string) {
	for _, token := range tokens {
		if err := s.db.Where("token = ?", token).Delete(&models.Device{}).Error; err != nil {
			log.Printf("Error cleaning up invalid token %s: %v", token, err)
		} else {
			log.Printf("Cleaned up invalid token: %s", token)
		}
	}
}

// NewChatHandler initializes a new chat handler
func NewChatHandler(db *gorm.DB) *ChatHandler {
	hub := models.NewHub()
	go hub.Run()

	notificationSender := &DefaultNotificationSender{
		db:         db,
		expoClient: expo.NewPushClient(nil),
	}

	return &ChatHandler{
		db:                 db,
		hub:                hub,
		notificationSender: notificationSender,
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	HandshakeTimeout: 10 * time.Second, // Add a reasonable timeout
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (h *ChatHandler) RegisterRoutes(router *mux.Router) {
	// WebSocket connection
	router.HandleFunc("/ws/{id}", h.HandleWebSocket)

	// Channel routes
	router.HandleFunc("/channels", utils.AuthMiddleware(h.CreateChannel)).Methods("POST")
	router.HandleFunc("/channels", h.GetChannels).Methods("GET")
	router.HandleFunc("/channels/{id}", h.GetChannel).Methods("GET")
	router.HandleFunc("/channels/{id}/join", utils.AuthMiddleware(h.JoinChannel)).Methods("POST")
	router.HandleFunc("/channels/{id}/members", utils.AuthMiddleware(h.GetChannelMembers)).Methods("GET")
	router.HandleFunc("/channels/{id}/admins", utils.AuthMiddleware(h.GetChannelAdmins)).Methods("GET")
	router.HandleFunc("/channels/{id}/admins", utils.AuthMiddleware(h.AddChannelAdmin)).Methods("POST")
	router.HandleFunc("/channels/{id}/admins", utils.AuthMiddleware(h.RemoveChannelAdmin)).Methods("DELETE")

	// Message routes
	router.HandleFunc("/messages/peer/{userId}", utils.AuthMiddleware(h.GetPeerMessages)).Methods("GET")
	router.HandleFunc("/channels/{id}/messages", utils.AuthMiddleware(h.GetChannelMessages)).Methods("GET")
}

// HandleWebSocket handles WebSocket connections
func (h *ChatHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Println("WebSocket connection request received")

	vars := mux.Vars(r)
	UserID, err := strconv.ParseUint(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}

	// Set a reasonable timeout for the WebSocket upgrade
	upgrader.HandshakeTimeout = 5 * time.Second

	// Establish the connection first, before doing any database operations
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v\n", err)
		return
	}

	log.Printf("WebSocket connection established for user %d\n", UserID)

	// Create the client
	client := &models.ClientConnection{
		Hub:    h.hub,
		Conn:   conn,
		Send:   make(chan []byte, 256),
		UserID: uint(UserID),
	}

	// Register the client immediately to establish connection quickly
	h.hub.Register <- client

	// Start the write pump to handle sending messages to the client
	go client.WritePump()

	// Start the read pump to handle incoming messages from the client
	go h.handleClientMessages(client)

	// Asynchronously load and subscribe to channels after connection is established
	go func() {
		var channels []models.Channel

		err := h.db.Joins("JOIN channel_clients ON channels.id = channel_clients.channel_id").
			Joins("JOIN clients ON channel_clients.client_id = clients.id").
			Where("clients.user_id = ?", UserID).
			Find(&channels).Error

		if err != nil {
			log.Printf("Error loading channels for user %d: %v\n", UserID, err)
			return
		}

		log.Printf("Subscribing user %d to %d channels\n", UserID, len(channels))

		for _, channel := range channels {
			h.hub.SubscribeToChannel(channel.ID, client)
		}

		// Send a confirmation message to the client
		confirmMsg := models.WebSocketMessage{
			Type:       "connection_established",
			PeerMsg:    nil,
			ChannelMsg: nil,
		}

		if msgBytes, err := json.Marshal(confirmMsg); err == nil {
			client.Send <- msgBytes
		}
	}()
}

func (h *ChatHandler) validateMessage(msg *models.WebSocketMessage, senderID uint) error {
	switch msg.Type {
	case models.PeerMessageType:
		if msg.PeerMsg == nil {
			return errors.New("peer message is nil")
		}
		if msg.PeerMsg.ReceiverID == 0 {
			return errors.New("invalid receiver ID")
		}
		if msg.PeerMsg.Content == "" {
			return errors.New("message content cannot be empty")
		}
	case models.ChannelMessageType:
		if msg.ChannelMsg == nil {
			return errors.New("channel message is nil")
		}
		if msg.ChannelMsg.ChannelID == 0 {
			return errors.New("invalid channel ID")
		}
		if msg.ChannelMsg.Content == "" {
			return errors.New("message content cannot be empty")
		}

		// Check if user is an admin of the channel
		var count int64
		h.db.Model(&models.Channel{}).
			Joins("JOIN channel_admins ON channels.id = channel_admins.channel_id").
			Joins("JOIN clients ON channel_admins.client_id = clients.id").
			Where("channels.id = ? AND clients.user_id = ?", msg.ChannelMsg.ChannelID, senderID).
			Count(&count)

		if count == 0 {
			return errors.New("only channel admins can send messages")
		}
	default:
		return errors.New("invalid message type")
	}
	return nil
}

func (h *ChatHandler) handleClientMessages(client *models.ClientConnection) {
	defer func() {
		h.hub.Unregister <- client
		client.Conn.Close()
	}()

	for {
		_, message, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		var wsMsg models.WebSocketMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			log.Printf("error unmarshaling message: %v", err)
			continue
		}

		if err := h.validateMessage(&wsMsg, client.UserID); err != nil {
			log.Printf("message validation failed: %v", err)
			// Send error message back to client
			errorMsg := models.WebSocketMessage{
				Type: models.MessageType(fmt.Sprintf("error: %v", err.Error())),
			}
			if msgBytes, err := json.Marshal(errorMsg); err == nil {
				client.Send <- msgBytes
			} else {
				log.Printf("failed to marshal error message: %v", err)
			}
			continue
		}

		switch wsMsg.Type {
		case models.PeerMessageType:
			if wsMsg.PeerMsg == nil {
				continue
			}
			wsMsg.PeerMsg.SenderID = client.UserID
			wsMsg.PeerMsg.CreatedAt = time.Now()

			// Save to database
			if err := h.db.Create(wsMsg.PeerMsg).Error; err != nil {
				log.Printf("error saving peer message: %v", err)
				continue
			}

			// Get sender's information for notification
			var sender models.Client
			if err := h.db.Where("user_id = ?", client.UserID).First(&sender).Error; err != nil {
				log.Printf("error getting sender info: %v", err)
			}

			// Send notification to receiver
			receiverUserID := strconv.FormatUint(uint64(wsMsg.PeerMsg.ReceiverID), 10)
			senderName := fmt.Sprintf("User %d", client.UserID) // Default name if client info not found
			if sender.ID != 0 {
				// If you have a name field in your Client model, use it here
				// senderName = sender.Name
			}

			// Create notification data
			notificationData := map[string]interface{}{
				"messageType": "peer",
				"messageId":   wsMsg.PeerMsg.ID,
				"senderId":    wsMsg.PeerMsg.SenderID,
				"timestamp":   wsMsg.PeerMsg.CreatedAt.Unix(),
			}

			// Send notification
			go func(receiverID string, senderName string, content string, data map[string]interface{}) {
				title := fmt.Sprintf("New message from %s", senderName)

				// Truncate content if too long for notification
				messagePreview := content
				if len(messagePreview) > 100 {
					messagePreview = messagePreview[:97] + "..."
				}

				success, err := h.notificationSender.SendUserNotification(receiverID, title, messagePreview, data)
				if !success || err != nil {
					log.Printf("Failed to send notification to user %s: %v", receiverID, err)
				}
			}(receiverUserID, senderName, wsMsg.PeerMsg.Content, notificationData)

			// Broadcast to recipient
			msgBytes, _ := json.Marshal(wsMsg)
			h.hub.BroadcastToUser(wsMsg.PeerMsg.ReceiverID, msgBytes)

		case models.ChannelMessageType:
			if wsMsg.ChannelMsg == nil {
				continue
			}
			wsMsg.ChannelMsg.SenderID = client.UserID
			wsMsg.ChannelMsg.CreatedAt = time.Now()

			// Save to database
			if err := h.db.Create(wsMsg.ChannelMsg).Error; err != nil {
				log.Printf("error saving channel message: %v", err)
				continue
			}

			// Get channel info
			var channel models.Channel
			if err := h.db.First(&channel, wsMsg.ChannelMsg.ChannelID).Error; err != nil {
				log.Printf("error getting channel info: %v", err)
				continue
			}

			// Get sender's information for notification
			var sender models.Client
			if err := h.db.Where("user_id = ?", client.UserID).First(&sender).Error; err != nil {
				log.Printf("error getting sender info: %v", err)
			}

			// Get all channel members for notifications
			var members []models.Client
			if err := h.db.Model(&channel).Association("Clients").Find(&members); err != nil {
				log.Printf("error getting channel members: %v", err)
				continue
			}

			senderName := fmt.Sprintf("User %d", client.UserID) // Default name if client info not found
			if sender.ID != 0 {
				// If you have a name field in your Client model, use it here
				// senderName = sender.Name
			}

			// Create notification data
			notificationData := map[string]interface{}{
				"messageType": "channel",
				"messageId":   wsMsg.ChannelMsg.ID,
				"channelId":   wsMsg.ChannelMsg.ChannelID,
				"channelName": channel.Name,
				"senderId":    wsMsg.ChannelMsg.SenderID,
				"timestamp":   wsMsg.ChannelMsg.CreatedAt.Unix(),
			}

			// Send notifications to all channel members (except the sender)
			for _, member := range members {
				// Skip the sender - they don't need a notification for their own message
				if member.UserID == client.UserID {
					continue
				}

				// Send notification
				go func(memberID uint, channelName, senderName, content string, data map[string]interface{}) {
					memberUserID := strconv.FormatUint(uint64(memberID), 10)
					title := fmt.Sprintf("New message in %s", channelName)

					// Format the body with sender name and content preview
					messagePreview := content
					if len(messagePreview) > 80 {
						messagePreview = messagePreview[:77] + "..."
					}
					body := fmt.Sprintf("%s: %s", senderName, messagePreview)

					success, err := h.notificationSender.SendUserNotification(memberUserID, title, body, data)
					if !success || err != nil {
						log.Printf("Failed to send notification to user %d: %v", memberID, err)
					}
				}(member.UserID, channel.Name, senderName, wsMsg.ChannelMsg.Content, notificationData)
			}

			// Broadcast to channel
			msgBytes, _ := json.Marshal(wsMsg)
			h.hub.BroadcastToChannel(wsMsg.ChannelMsg.ChannelID, msgBytes)
		}
	}
}

// CreateChannel handles channel creation
func (h *ChatHandler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	var channel models.Channel
	if err := json.NewDecoder(r.Body).Decode(&channel); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Start a transaction
	tx := h.db.Begin()

	// Create the channel
	if err := tx.Create(&channel).Error; err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Get the creator's user ID
	userID, err := utils.GetUserIDFromContext(r.Context())
	if err != nil {
		tx.Rollback()
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get or create client for the creator
	var client models.Client
	if err := tx.FirstOrCreate(&client, models.Client{UserID: userID}).Error; err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Add creator as both member and admin
	if err := tx.Model(&channel).Association("Clients").Append(&client); err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Model(&channel).Association("Admins").Append(&client); err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Commit the transaction
	if err := tx.Commit().Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(channel)
}

// GetChannels returns all available channels
func (h *ChatHandler) GetChannels(w http.ResponseWriter, r *http.Request) {
	var channels []models.Channel
	if err := h.db.Find(&channels).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(channels)
}

// GetChannel returns a specific channel
func (h *ChatHandler) GetChannel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	var channel models.Channel
	if err := h.db.First(&channel, channelID).Error; err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(channel)
}

// JoinChannel handles joining a channel
func (h *ChatHandler) JoinChannel(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	userID, err := utils.GetUserIDFromContext(r.Context())
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Get or create client
	var client models.Client
	result := h.db.FirstOrCreate(&client, models.Client{UserID: userID})
	if result.Error != nil {
		http.Error(w, result.Error.Error(), http.StatusInternalServerError)
		return
	}

	// Add client to channel
	var channel models.Channel
	if err := h.db.First(&channel, channelID).Error; err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := h.db.Model(&channel).Association("Clients").Append(&client); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// GetPeerMessages retrieves peer-to-peer messages
func (h *ChatHandler) GetPeerMessages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	peerID, err := strconv.ParseUint(vars["userId"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}

	userID, err := utils.GetUserIDFromContext(r.Context())
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var messages []models.PeerMessage
	if err := h.db.Where(
		"(sender_id = ? AND receiver_id = ?) OR (sender_id = ? AND receiver_id = ?)",
		userID, peerID, peerID, userID,
	).Order("created_at asc").Find(&messages).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(messages)
}

// GetChannelMessages retrieves messages from a channel
func (h *ChatHandler) GetChannelMessages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	// Parse pagination parameters
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit := 50 // Messages per page

	var messages []models.ChannelMessage
	var total int64

	// Get total count
	h.db.Model(&models.ChannelMessage{}).Where("channel_id = ?", channelID).Count(&total)

	// Get paginated messages
	if err := h.db.Where("channel_id = ?", channelID).
		Order("created_at desc").
		Limit(limit).
		Offset((page - 1) * limit).
		Find(&messages).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := struct {
		Messages []models.ChannelMessage `json:"messages"`
		Total    int64                   `json:"total"`
		Page     int                     `json:"page"`
		Pages    int                     `json:"pages"`
	}{
		Messages: messages,
		Total:    total,
		Page:     page,
		Pages:    int(math.Ceil(float64(total) / float64(limit))),
	}

	json.NewEncoder(w).Encode(response)
}

func (h *ChatHandler) GetChannelMembers(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	// Retrieve the channel from the database
	var channel models.Channel
	if err := h.db.First(&channel, channelID).Error; err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	// Retrieve the members of the channel
	var members []models.Client
	if err := h.db.Model(&channel).Association("Clients").Find(&members); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the members as a JSON response
	json.NewEncoder(w).Encode(members)
}

func (h *ChatHandler) AddChannelAdmin(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	var request struct {
		UserID uint `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Start a transaction
	tx := h.db.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Get the channel
	var channel models.Channel
	if err := tx.First(&channel, channelID).Error; err != nil {
		tx.Rollback()
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	// Get or create client
	var client models.Client
	if err := tx.Where(models.Client{UserID: request.UserID}).FirstOrCreate(&client).Error; err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Check if already an admin
	var count int64
	if err := tx.Model(&channel).
		Joins("JOIN channel_admins ON channel_admins.channel_id = ? AND channel_admins.client_id = ?",
			channel.ID, client.ID).
		Count(&count).Error; err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if count > 0 {
		tx.Rollback()
		http.Error(w, "User is already an admin", http.StatusBadRequest)
		return
	}

	// Add to admins using direct SQL to ensure proper association
	if err := tx.Exec("INSERT INTO channel_admins (channel_id, client_id) VALUES (?, ?)",
		channel.ID, client.ID).Error; err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Commit transaction
	if err := tx.Commit().Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response with the updated admin list
	var admins []models.Client
	if err := h.db.Model(&channel).Association("Admins").Find(&admins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(admins)
}

// Remove an admin from a channel
func (h *ChatHandler) RemoveChannelAdmin(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	var request struct {
		UserID uint `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var client models.Client
	if err := h.db.Where("user_id = ?", request.UserID).First(&client).Error; err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var channel models.Channel
	if err := h.db.First(&channel, channelID).Error; err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	if err := h.db.Model(&channel).Association("Admins").Delete(&client); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// Get channel admins
func (h *ChatHandler) GetChannelAdmins(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channelID, err := strconv.ParseUint(vars["id"], 10, 32)
	if err != nil {
		http.Error(w, "Invalid channel ID", http.StatusBadRequest)
		return
	}

	var channel models.Channel
	if err := h.db.First(&channel, channelID).Error; err != nil {
		http.Error(w, "Channel not found", http.StatusNotFound)
		return
	}

	var admins []models.Client
	if err := h.db.Model(&channel).Association("Admins").Find(&admins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(admins)
}
