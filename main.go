package main

import (
	"context"
	"log"
	"strings"

	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"ai_tg_bot/config"
)

const (
	mongoURI       = "mongodb://localhost:27017" // Change if needed
	databaseName   = "tg_openai_bot"
	collectionName = "chat_history"
	openAIAPIURL   = "https://api.openai.com/v1/chat/completions"
)

type ChatMessage struct {
	UserID  int64  `bson:"user_id"`
	Role    string `bson:"role"` // "user" or "assistant"
	Content string `bson:"content"`
}

type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
}

type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OpenAIResponse struct {
	Choices []struct {
		Message OpenAIMessage `json:"message"`
	} `json:"choices"`
}

func main() {
	cfg := config.LoadConfig()
	if cfg.TelegramBotToken == "" || cfg.OpenAIAPIKey == "" || cfg.MongoURI == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN, OPENAI_API_KEY and MONGO_URI environment variables must be set")
	}

	// Connect to MongoDB
	client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.TODO())

	collection := client.Database(databaseName).Collection(collectionName)

	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}

		userID := update.Message.From.ID
		text := update.Message.Text

		if strings.HasPrefix(text, "/start") {
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Привет! Отправь сообщение, и я отвечу с помощью OpenAI. Можно выбрать модель командой /model <имя_модели> (например, gpt-3.5-turbo). По умолчанию используется gpt-3.5-turbo.")
			bot.Send(msg)
			continue
		}

		if strings.HasPrefix(text, "/model") {
			parts := strings.Split(text, " ")
			if len(parts) < 2 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Пожалуйста, укажите имя модели после команды /model")
				bot.Send(msg)
				continue
			}
			model := parts[1]
			err := setUserModel(collection, userID, model)
			if err != nil {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Ошибка при сохранении модели")
				bot.Send(msg)
				continue
			}
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, fmt.Sprintf("Модель установлена на %s", model))
			bot.Send(msg)
			continue
		}

		go func(userID int64, chatID int64, text string) {
			model, err := getUserModel(collection, userID)
			if err != nil || model == "" {
				model = "gpt-3.5-turbo"
			}

			// Load chat history
			history, err := loadChatHistory(collection, userID)
			if err != nil {
				log.Printf("Failed to load chat history: %v", err)
			}

			// Append user message to history
			history = append(history, ChatMessage{
				UserID:  userID,
				Role:    "user",
				Content: text,
			})

			// Prepare messages for OpenAI
			var messages []OpenAIMessage
			for _, msg := range history {
				messages = append(messages, OpenAIMessage{
					Role:    msg.Role,
					Content: msg.Content,
				})
			}

			// Call OpenAI API
			responseText, err := callOpenAI(cfg.OpenAIAPIKey, model, messages)
			if err != nil {
				msg := tgbotapi.NewMessage(chatID, "Ошибка при обращении к OpenAI API")
				bot.Send(msg)
				return
			}

			// Append assistant response to history
			history = append(history, ChatMessage{
				UserID:  userID,
				Role:    "assistant",
				Content: responseText,
			})

			// Save updated history
			err = saveChatHistory(collection, userID, history)
			if err != nil {
				log.Printf("Failed to save chat history: %v", err)
			}

			// Send response to user
			msg := tgbotapi.NewMessage(chatID, responseText)
			bot.Send(msg)
		}(userID, update.Message.Chat.ID, text)
	}
}

func setUserModel(collection *mongo.Collection, userID int64, model string) error {
	filter := bson.M{"user_id": userID, "type": "model"}
	update := bson.M{"$set": bson.M{"model": model}}
	opts := options.Update().SetUpsert(true)
	_, err := collection.UpdateOne(context.TODO(), filter, update, opts)
	return err
}

func getUserModel(collection *mongo.Collection, userID int64) (string, error) {
	filter := bson.M{"user_id": userID, "type": "model"}
	var result struct {
		Model string `bson:"model"`
	}
	err := collection.FindOne(context.TODO(), filter).Decode(&result)
	if err != nil {
		return "", err
	}
	return result.Model, nil
}

func loadChatHistory(collection *mongo.Collection, userID int64) ([]ChatMessage, error) {
	filter := bson.M{"user_id": userID, "type": "chat"}
	cursor, err := collection.Find(context.TODO(), filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.TODO())

	var history []ChatMessage
	for cursor.Next(context.TODO()) {
		var msg ChatMessage
		err := cursor.Decode(&msg)
		if err != nil {
			return nil, err
		}
		history = append(history, msg)
	}
	return history, nil
}

func saveChatHistory(collection *mongo.Collection, userID int64, history []ChatMessage) error {
	// Remove old chat history for user
	_, err := collection.DeleteMany(context.TODO(), bson.M{"user_id": userID, "type": "chat"})
	if err != nil {
		return err
	}

	// Insert updated history with type "chat"
	var docs []interface{}
	for _, msg := range history {
		doc := bson.M{
			"user_id": userID,
			"role":    msg.Role,
			"content": msg.Content,
			"type":    "chat",
		}
		docs = append(docs, doc)
	}
	_, err = collection.InsertMany(context.TODO(), docs)
	return err
}

func callOpenAI(apiKey, model string, messages []OpenAIMessage) (string, error) {
	reqBody := OpenAIRequest{
		Model:    model,
		Messages: messages,
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", openAIAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var openAIResp OpenAIResponse
	err = json.NewDecoder(resp.Body).Decode(&openAIResp)
	if err != nil {
		return "", err
	}

	if len(openAIResp.Choices) > 0 {
		return openAIResp.Choices[0].Message.Content, nil
	}
	return "", fmt.Errorf("no response from OpenAI")
}
