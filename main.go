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

	firstRunMap   = make(map[string]bool)
	firstRunMapMu sync.Mutex

	sharedHTTP = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     30 * time.Second,
			DisableKeepAlives:   false,
		},
	}

	otpRegex = regexp.MustCompile(`\b\d{3,4}[-\s]?\d{3,4}\b|\b\d{4,8}\b`)
)

// ── MongoDB ──────────────────────────────
func initMongoDB() {
	// ✅ Railway auto-detect MongoDB URL
	uri := os.Getenv("MONGO_URL")
	if uri == "" { uri = os.Getenv("MONGODB_URL") }
	if uri == "" { uri = os.Getenv("MONGODB_PRIVATE_URL") }
	if uri == "" { uri = os.Getenv("MONGODB_URI") }
	if uri == "" {
		uri = "mongodb://mongo:AcOOyioCfLYnfygdxtXQUqDXYuykCkoH@mongodb.railway.internal:27017"
	}
	fmt.Printf("🍃 MongoDB: %.40s...
", uri)

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
	fmt.Println("✅ MongoDB connected")
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

// ── Helpers ──────────────────────────────
func extractOTP(msg string) string       { return otpRegex.FindString(msg) }
func maskPhone(phone string) string {
	if len(phone) < 6 { return phone }
	return phone[:3] + "•••" + phone[len(phone)-4:]
}
func cleanCountry(name string) string {
	if name == "" { return "Unknown" }
	p := strings.Fields(strings.Split(name, "-")[0])
	if len(p) > 0 { return p[0] }
	return "Unknown"
}

// ── Send to all channels (parallel) ─────
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
			if err != nil { return }
			_, _ = client.SendMessage(context.Background(), jid, &waProto.Message{
				Conversation: proto.String(strings.TrimSpace(msg)),
			})
		}(jidStr)
	}
	wg.Wait()
}

// ── Per-API worker ────────────────────────
func startAPIWorker(apiURL string, idx int) {
	firstRunMapMu.Lock()
	firstRunMap[apiURL] = true
	firstRunMapMu.Unlock()

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
	if err != nil { return false }
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil { return false }
	if data == nil || data["aaData"] == nil { return true }

	rows, ok := data["aaData"].([]interface{})
	if !ok || len(rows) == 0 { return true }

	firstRunMapMu.Lock()
	isFirst := firstRunMap[apiURL]
	firstRunMapMu.Unlock()

	if isFirst {
		for _, row := range rows {
			r, ok := row.([]interface{})
			if !ok || len(r) < 3 { continue }
			id := fmt.Sprintf("%v_%v", r[2], r[0])
			if !isAlreadySent(id) { markAsSent(id) }
		}
		firstRunMapMu.Lock()
		firstRunMap[apiURL] = false
		firstRunMapMu.Unlock()
		fmt.Printf("✅ [API %d] First run: %d msgs marked\n", idx, len(rows))

		return true
	}

	var wg sync.WaitGroup
	for _, row := range rows {
		r, ok := row.([]interface{})
		if !ok || len(r) < 5 { continue }

		ts      := fmt.Sprintf("%v", r[0])
		country := fmt.Sprintf("%v", r[1])
		phone   := fmt.Sprintf("%v", r[2])
		service := fmt.Sprintf("%v", r[3])
		msg     := fmt.Sprintf("%v", r[4])

		if phone == "0" || phone == "" { continue }

		msgID := fmt.Sprintf("%v_%v", phone, ts)
		if isAlreadySent(msgID) { continue }
		markAsSent(msgID) // ✅ Mark before goroutine - no duplicate sends

		wg.Add(1)
		go func(msgID, ts, country, phone, service, msg string) {
			defer wg.Done()

			cn    := cleanCountry(country)
			flag, _ := GetCountryWithFlag(cn)
			otp   := extractOTP(msg)
			flat  := strings.ReplaceAll(strings.ReplaceAll(msg, "\n", " "), "\r", "")

			body := fmt.Sprintf(
				"✨ *%s | %s Message %d* ⚡\n\n"+
				"> *Time:* %s\n"+
				"> *Country:* %s %s\n"+
				"   *Number:* *%s*\n"+
				"> *Service:* %s\n"+
				"   *OTP:* *%s*\n\n"+
				"> *Join For Numbers:*\n"+
				"> ¹ https://chat.whatsapp.com/EbaJKbt5J2T6pgENIeFFht\n"+
				"> ² https://chat.whatsapp.com/L0Qk2ifxRFU3fduGA45osD\n\n"+
				"*Full Message:*\n%s\n\n"+
				"> © Developed by Nothing Is Impossible",
				flag, strings.ToUpper(service), idx,
				ts, flag, cn, maskPhone(phone),
				service, otp, flat,
			)
			sendToChannels(body)
			fmt.Printf("✅ [API %d] %s %s | %s | OTP: %s\n", idx, flag, cn, maskPhone(phone), otp)
		}(msgID, ts, country, phone, service, msg)
	}
	wg.Wait()
	return true
}

// ── WhatsApp Events ───────────────────────
func handler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if !v.Info.IsFromMe { handleIDCommand(v) }
	case *events.LoggedOut:
		fmt.Println("⚠️  WhatsApp logged out!")
	case *events.Disconnected:
		fmt.Println("❌ Disconnected, reconnecting...")
		go func() {
			time.Sleep(3 * time.Second)
			if client != nil { _ = client.Connect() }
		}()
	case *events.Connected:
		fmt.Println("✅ WhatsApp connected")
	}
}

func handleIDCommand(evt *events.Message) {
	text := evt.Message.GetConversation()
	if text == "" && evt.Message.ExtendedTextMessage != nil {
		text = evt.Message.ExtendedTextMessage.GetText()
	}
	if strings.TrimSpace(strings.ToLower(text)) != ".id" { return }

	resp := fmt.Sprintf(
		"👤 *User ID:*\n`%s`\n\n📍 *Chat ID:*\n`%s`",
		evt.Info.Sender.ToNonAD().String(),
		evt.Info.Chat.ToNonAD().String(),
	)
	if evt.Message.ExtendedTextMessage != nil &&
		evt.Message.ExtendedTextMessage.ContextInfo != nil &&
		evt.Message.ExtendedTextMessage.ContextInfo.Participant != nil {
		q := strings.Split(*evt.Message.ExtendedTextMessage.ContextInfo.Participant, ":")[0]
		resp += fmt.Sprintf("\n\n↩️ *Replied ID:*\n`%s`", q)
	}
	if client != nil {
		_, _ = client.SendMessage(context.Background(), evt.Info.Chat, &waProto.Message{
			Conversation: proto.String(resp),
		})
	}
}

// ── HTTP Endpoints ────────────────────────
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
%v\n", err)
	} else {
		if dev, err := container.GetFirstDevice(context.Background()); err == nil {
			client = whatsmeow.NewClient(dev, waLog.Stdout("WA", "INFO", true))
			client.AddEventHandler(handler)
			if client.Store.ID != nil {
				if err := client.Connect(); err == nil {
					fmt.Println("✅ Session restored")
				}
			}
		}
	}

	// ✅ Start all API workers in parallel
	fmt.Printf("🔄 Starting %d API workers (interval: %ds)...\n", len(Config.OTPApiURLs), Config.Interval)
	for i, url := range Config.OTPApiURLs {
		go startAPIWorker(url, i+1)
		time.Sleep(100 * time.Millisecond) // slight stagger
	}
	fmt.Println("✅ All workers running!\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\n🛑 Shutting down...")
	if client != nil { client.Disconnect() }
}
