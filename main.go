package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/protobuf/proto"
)

var (
	client    *whatsmeow.Client
	container *sqlstore.Container
	mongoColl *mongo.Collection

	seenCache   = make(map[string]struct{})
	seenCacheMu sync.RWMutex

	sharedHTTP = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	otpRegex = regexp.MustCompile(`\b\d{3,4}[-\s]?\d{3,4}\b|\b\d{4,8}\b`)
)

// ── MongoDB ──────────────────────────────────────────────────────────────────

func initMongoDB() {
	uri := os.Getenv("MONGO_URL")
	if uri == "" {
		uri = os.Getenv("MONGODB_URL")
	}
	if uri == "" {
		uri = os.Getenv("MONGODB_PRIVATE_URL")
	}
	if uri == "" {
		uri = os.Getenv("MONGODB_URI")
	}
	if uri == "" {
		uri = "mongodb://mongo:AcOOyioCfLYnfygdxtXQUqDXYuykCkoH@mongodb.railway.internal:27017"
	}

	preview := uri
	if len(preview) > 40 {
		preview = preview[:40] + "..."
	}
	fmt.Println("MongoDB: " + preview)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mc, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		panic(fmt.Sprintf("MongoDB failed: %v", err))
	}
	mongoColl = mc.Database("kami_otp_db").Collection("sent_otps")
	_, _ = mongoColl.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys:    bson.M{"msg_id": 1},
		Options: options.Index().SetUnique(true),
	})
	fmt.Println("MongoDB connected")
}

func isAlreadySent(id string) bool {
	seenCacheMu.RLock()
	_, ok := seenCache[id]
	seenCacheMu.RUnlock()
	if ok {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var r bson.M
	return mongoColl.FindOne(ctx, bson.M{"msg_id": id}).Decode(&r) == nil
}

func markAsSent(id string) {
	seenCacheMu.Lock()
	seenCache[id] = struct{}{}
	if len(seenCache) > 10000 {
		for k := range seenCache {
			delete(seenCache, k)
			break
		}
	}
	seenCacheMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = mongoColl.InsertOne(ctx, bson.M{"msg_id": id, "at": time.Now()})
	}()
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func extractOTP(msg string) string {
	return otpRegex.FindString(msg)
}

func maskPhone(phone string) string {
	if len(phone) < 6 {
		return phone
	}
	return phone[:3] + "..." + phone[len(phone)-4:]
}

func cleanCountry(name string) string {
	if name == "" {
		return "Unknown"
	}
	p := strings.Fields(strings.Split(name, "-")[0])
	if len(p) > 0 {
		return strings.Join(p, " ")
	}
	return "Unknown"
}

// ── Send to all channels parallel ────────────────────────────────────────────

func sendToChannels(msg string) {
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return
	}
	var wg sync.WaitGroup
	for _, jidStr := range Config.OTPChannelIDs {
		wg.Add(1)
		go func(j string) {
			defer wg.Done()
			jid, err := types.ParseJID(j)
			if err != nil {
				return
			}
			_, _ = client.SendMessage(context.Background(), jid, &waProto.Message{
				Conversation: proto.String(strings.TrimSpace(msg)),
			})
		}(jidStr)
	}
	wg.Wait()
}

// ── Per-API worker ────────────────────────────────────────────────────────────

func startAPIWorker(apiURL string, idx int) {
	errStreak := 0
	for {
		if client != nil && client.IsConnected() && client.IsLoggedIn() {
			if fetchAndProcess(apiURL, idx) {
				errStreak = 0
			} else {
				errStreak++
			}
		}
		sleep := time.Duration(Config.Interval) * time.Second
		if errStreak > 5 {
			sleep = 15 * time.Second
		}
		time.Sleep(sleep)
	}
}

func fetchAndProcess(apiURL string, idx int) bool {
	resp, err := sharedHTTP.Get(apiURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}
	if data == nil || data["aaData"] == nil {
		return true
	}

	rows, ok := data["aaData"].([]interface{})
	if !ok || len(rows) == 0 {
		return true
	}

	var wg sync.WaitGroup
	for _, row := range rows {
		r, ok := row.([]interface{})
		if !ok || len(r) < 5 {
			continue
		}

		ts      := fmt.Sprintf("%v", r[0])
		country := fmt.Sprintf("%v", r[1])
		phone   := fmt.Sprintf("%v", r[2])
		service := fmt.Sprintf("%v", r[3])
		msg     := fmt.Sprintf("%v", r[4])

		if phone == "0" || phone == "" {
			continue
		}

		msgID := fmt.Sprintf("%v_%v", phone, ts)
		if isAlreadySent(msgID) {
			continue
		}
		markAsSent(msgID)

		wg.Add(1)
		go func(msgID, ts, country, phone, service, msg string) {
			defer wg.Done()

			cn := cleanCountry(country)
			flag, _ := GetCountryWithFlag(cn)
			otp := extractOTP(msg)
			flat := strings.ReplaceAll(strings.ReplaceAll(msg, "\n", " "), "\r", "")

			line1 := "✨ *" + flag + " | " + strings.ToUpper(service) + " Message " + fmt.Sprintf("%d", idx) + "* ⚡"
			line2 := "> *Time:* " + ts
			line3 := "> *Country:* " + flag + " " + cn
			line4 := "   *Number:* *" + maskPhone(phone) + "*"
			line5 := "> *Service:* " + service
			line6 := "   *OTP:* *" + otp + "*"
			line7 := "> *Join For Numbers:*"
			line8 := "> 1 https://chat.whatsapp.com/EbaJKbt5J2T6pgENIeFFht"
			line9 := "> 2 https://chat.whatsapp.com/L0Qk2ifxRFU3fduGA45osD"
			line10 := "*Full Message:*"
			line11 := flat
			line12 := "> Developed by Nothing Is Impossible"

			body := strings.Join([]string{
				line1, "",
				line2, line3, line4,
				line5, line6, "",
				line7, line8, line9, "",
				line10, line11, "",
				line12,
			}, "\n")

			sendToChannels(body)
			fmt.Printf("Sent API %d: %s %s | OTP: %s\n", idx, flag, cn, otp)
		}(msgID, ts, country, phone, service, msg)
	}
	wg.Wait()
	return true
}

// ── WhatsApp Events ───────────────────────────────────────────────────────────

func handler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if !v.Info.IsFromMe {
			handleIDCommand(v)
		}
	case *events.LoggedOut:
		fmt.Println("WhatsApp logged out!")
	case *events.Disconnected:
		fmt.Println("Disconnected, reconnecting...")
		go func() {
			time.Sleep(3 * time.Second)
			if client != nil {
				_ = client.Connect()
			}
		}()
	case *events.Connected:
		fmt.Println("WhatsApp connected")
	}
}

func handleIDCommand(evt *events.Message) {
	text := evt.Message.GetConversation()
	if text == "" && evt.Message.ExtendedTextMessage != nil {
		text = evt.Message.ExtendedTextMessage.GetText()
	}
	if strings.TrimSpace(strings.ToLower(text)) != ".id" {
		return
	}

	resp := "User ID:\n" + evt.Info.Sender.ToNonAD().String() + "\n\nChat ID:\n" + evt.Info.Chat.ToNonAD().String()

	if evt.Message.ExtendedTextMessage != nil &&
		evt.Message.ExtendedTextMessage.ContextInfo != nil &&
		evt.Message.ExtendedTextMessage.ContextInfo.Participant != nil {
		q := strings.Split(*evt.Message.ExtendedTextMessage.ContextInfo.Participant, ":")[0]
		resp += "\n\nReplied ID:\n" + q
	}

	if client != nil {
		_, _ = client.SendMessage(context.Background(), evt.Info.Chat, &waProto.Message{
			Conversation: proto.String(resp),
		})
	}
}

// ── HTTP Endpoints ────────────────────────────────────────────────────────────

func handlePairAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, `{"error":"Use: /link/pair/NUMBER"}`, 400)
		return
	}
	number := strings.NewReplacer("+", "", " ", "", "-", "").Replace(strings.TrimSpace(parts[3]))
	if len(number) < 10 || len(number) > 15 {
		http.Error(w, `{"error":"Invalid number"}`, 400)
		return
	}
	fmt.Println("Pair request: " + number)

	if client != nil && client.IsConnected() {
		client.Disconnect()
		time.Sleep(2 * time.Second)
	}

	tmp := whatsmeow.NewClient(container.NewDevice(), waLog.Stdout("Pair", "INFO", true))
	tmp.AddEventHandler(handler)
	if err := tmp.Connect(); err != nil {
		http.Error(w, `{"error":"Connect failed"}`, 500)
		return
	}
	time.Sleep(3 * time.Second)

	code, err := tmp.PairPhone(context.Background(), number, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		tmp.Disconnect()
		http.Error(w, `{"error":"Pair failed"}`, 500)
		return
	}
	fmt.Println("Pairing code: " + code)

	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(1 * time.Second)
			if tmp.Store.ID != nil {
				fmt.Println("Paired successfully!")
				client = tmp
				return
			}
		}
		fmt.Println("Pair timeout")
		tmp.Disconnect()
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"success": "true", "code": code, "number": number})
}

func handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if client != nil && client.IsConnected() {
		client.Disconnect()
	}
	devices, _ := container.GetAllDevices(context.Background())
	for _, d := range devices {
		_ = d.Delete(context.Background())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"success": "true", "message": "Session deleted"})
}

// ── MAIN ──────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("Kami OTP Bot starting...")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Kami OTP Bot Running | /link/pair/NUMBER to pair")
	})
	http.HandleFunc("/link/pair/", handlePairAPI)
	http.HandleFunc("/link/delete", handleDeleteSession)

	go func() {
		fmt.Println("HTTP server: 0.0.0.0:" + port)
		if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil {
			fmt.Println("HTTP error: " + err.Error())
			os.Exit(1)
		}
	}()

	initMongoDB()

	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		dbURL = os.Getenv("POSTGRES_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("POSTGRESQL_URL")
	}
	if dbURL == "" {
		// Railway PostgreSQL fallback
		dbURL = "postgresql://postgres:eBkYBnunXyBZlViHGfkztDGHCwMbwnXq@postgres.railway.internal:5432/railway"
	}
	dbType := "postgres"

	var err error
	container, err = sqlstore.New(context.Background(), dbType, dbURL, waLog.Stdout("DB", "INFO", true))
	if err != nil {
		fmt.Println("DB error: " + err.Error())
	} else {
		if dev, err := container.GetFirstDevice(context.Background()); err == nil {
			client = whatsmeow.NewClient(dev, waLog.Stdout("WA", "INFO", true))
			client.AddEventHandler(handler)
			if client.Store.ID != nil {
				if err := client.Connect(); err == nil {
					fmt.Println("Session restored")
				}
			}
		}
	}

	fmt.Printf("Starting %d API workers...\n", len(Config.OTPApiURLs))
	for i, url := range Config.OTPApiURLs {
		go startAPIWorker(url, i+1)
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("All workers running!")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("Shutting down...")
	if client != nil {
		client.Disconnect()
	}
}
