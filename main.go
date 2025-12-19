package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const (
	openAIRealtimeURL = "wss://api.openai.com/v1/realtime?model=gpt-realtime"
	// 你的 client 音訊規格（你目前做的是 PCM16 16kHz mono）
	inRateHz  = 24000
	outRateHz = 24000

	// 「一句話結束」的簡易判斷：多久沒新音訊就 commit+response.create
	idleCommitAfter = 600 * time.Millisecond

	// WS keepalive
	pongWait   = 30 * time.Second
	pingPeriod = 10 * time.Second
	writeWait  = 5 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type openAIEvent map[string]any

func main() {

	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	if os.Getenv("OPENAI_API_KEY") == "" {
		log.Fatal("missing env OPENAI_API_KEY")
	}

	http.HandleFunc("/ws", handleClientWS)
	log.Println("listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleClientWS(w http.ResponseWriter, r *http.Request) {
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade error:", err)
		return
	}
	defer clientConn.Close()

	log.Println("client connected")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// client ws keepalive
	_ = clientConn.SetReadDeadline(time.Now().Add(pongWait))
	clientConn.SetPongHandler(func(string) error {
		_ = clientConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go pingLoop(ctx, clientConn)

	// connect to OpenAI Realtime WS
	openaiConn, err := dialOpenAIRealtime()
	if err != nil {
		log.Println("dial openai error:", err)
		return
	}
	defer openaiConn.Close()

	// OpenAI ws keepalive（可選，但建議）
	_ = openaiConn.SetReadDeadline(time.Now().Add(pongWait))
	openaiConn.SetPongHandler(func(string) error {
		_ = openaiConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go pingLoop(ctx, openaiConn)

	// 送 session.update：把 input/output 都改成 16kHz，並關掉 server_vad（我們用 idle commit）
	// session.created 的結構顯示 audio.input/output.format.type=audio/pcm、rate=24000 預設值 :contentReference[oaicite:10]{index=10}
	// client events 也說 session.update 可更新多數欄位、turn_detection 可用 null 清除 :contentReference[oaicite:11]{index=11}
	sendJSON(openaiConn, openAIEvent{
		"type": "session.update",
		"session": openAIEvent{
			"type": "realtime",
			// 讓模型用中文回（你也可以放更完整 system 指令）
			"instructions": "You are a helpful assistant that speaks in Traditional Chinese.",
			"audio": openAIEvent{
				"input": openAIEvent{
					"format": openAIEvent{"type": "audio/pcm", "rate": inRateHz},
					// 關掉 server VAD，改用我們自己的 idle commit（也可以不關，讓它自動 commit/create_response）
					"turn_detection": nil,
				},
				"output": openAIEvent{
					"format": openAIEvent{"type": "audio/pcm", "rate": outRateHz},
					"voice":  "alloy",
					"speed":  1,
				},
			},
			"output_modalities": []string{"audio"},
		},
	})
	log.Println("→ session.update sent")

	// 從 OpenAI 收到 audio delta 就轉回 binary 給 client
	var writeMu sync.Mutex // gorilla/websocket 不建議多 goroutine 同時 Write
	go func() {
		for {
			_, msg, err := openaiConn.ReadMessage()
			if err != nil {
				log.Println("openai read error:", err)
				cancel()
				return
			}

			var evt openAIEvent
			if err := json.Unmarshal(msg, &evt); err != nil {
				log.Println("openai json error:", err)
				continue
			}

			t, _ := evt["type"].(string)

			switch t {
			case "error":
				pretty, _ := json.MarshalIndent(evt, "", "  ")
				log.Printf("❌ openai error event:\n%s\n", string(pretty))
				// 也可以把錯誤送回 client（文字）
				writeMu.Lock()
				_ = clientConn.WriteMessage(websocket.TextMessage, pretty)
				writeMu.Unlock()

			case "response.output_audio.delta":
				// server events 定義：delta 是 base64 音訊 :contentReference[oaicite:12]{index=12}
				delta, _ := evt["delta"].(string)
				pcm, err := base64.StdEncoding.DecodeString(delta)
				if err != nil {
					log.Println("decode delta error:", err)
					continue
				}
				writeMu.Lock()
				_ = clientConn.WriteMessage(websocket.BinaryMessage, pcm)
				writeMu.Unlock()

			case "response.done":
				// response.done 一定會出現，代表這次回覆結束 :contentReference[oaicite:13]{index=13}
				log.Println("🟢 response.done")

			default:
				// 初期建議先觀察有哪些事件（session.created / session.updated / response.created…）
				log.Println("openai event:", t)
			}
		}
	}()

	// client → OpenAI：收到 binary 就 append；idle 一段時間就 commit + response.create
	var bytesSinceCommit int
	idleTimer := time.NewTimer(idleCommitAfter)
	idleTimer.Stop()

	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleCommitAfter)
	}

	// idle 時觸發 commit+response.create（commit 若 buffer 空會報錯，所以我們用 bytesSinceCommit 擋）:contentReference[oaicite:14]{index=14}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-idleTimer.C:
				if bytesSinceCommit > 0 {
					log.Println("→ idle: commit + response.create")
					sendJSON(openaiConn, openAIEvent{"type": "input_audio_buffer.commit"})
					// response.create 會觸發推理並開始回覆 :contentReference[oaicite:15]{index=15}
					sendJSON(openaiConn, openAIEvent{
						"type": "response.create",
						"response": openAIEvent{
							"output_modalities": []string{"audio"},
						},
					})
					bytesSinceCommit = 0
				}
			}
		}
	}()

	for {
		msgType, data, err := clientConn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				log.Println("client disconnected")
			} else {
				log.Println("client read error:", err)
			}
			cancel()
			return
		}

		switch msgType {
		case websocket.BinaryMessage:
			// append audio bytes（Base64）:contentReference[oaicite:16]{index=16}
			sendJSON(openaiConn, openAIEvent{
				"type":  "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(data),
			})
			bytesSinceCommit += len(data)
			resetIdle()

		case websocket.TextMessage:
			// 可選：手動控制
			cmd := string(data)
			switch cmd {
			case "commit":
				if bytesSinceCommit > 0 {
					log.Println("→ cmd commit + response.create")
					sendJSON(openaiConn, openAIEvent{"type": "input_audio_buffer.commit"})
					sendJSON(openaiConn, openAIEvent{"type": "response.create", "response": openAIEvent{"output_modalities": []string{"audio"}}})
					bytesSinceCommit = 0
				}
			case "clear":
				log.Println("→ cmd clear")
				sendJSON(openaiConn, openAIEvent{"type": "input_audio_buffer.clear"})
				bytesSinceCommit = 0
			case "cancel":
				log.Println("→ cmd response.cancel")
				sendJSON(openaiConn, openAIEvent{"type": "response.cancel"})
			default:
				log.Println("client text:", cmd)
			}
		}
	}
}

func dialOpenAIRealtime() (*websocket.Conn, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	h := http.Header{}
	// WebSocket guide：server-to-server 用標準 API key + Authorization header :contentReference[oaicite:17]{index=17}
	h.Set("Authorization", "Bearer "+apiKey)

	conn, _, err := websocket.DefaultDialer.Dial(openAIRealtimeURL, h)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func sendJSON(conn *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	_ = conn.WriteMessage(websocket.TextMessage, b)
}

func pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
