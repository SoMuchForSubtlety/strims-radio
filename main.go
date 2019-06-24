package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/youtube/v3"
)

type bot struct {
	mu         sync.Mutex
	authToken  string
	address    string
	conn       *websocket.Conn
	lastPublic time.Time
	client     *http.Client
	ytServ     *youtube.Service
}

type message struct {
	Type     string `json:"type"`
	Contents *contents
}

type contents struct {
	Nick      string `json:"nick"`
	Data      string `json:"data"`
	Timestamp int64  `json:"timestamp"`
}

type config struct {
	AuthToken            string `json:"auth_token"`
	Address              string `json:"address"`
	CalendarClientID     string `json:"calendar_client_ID"`
	CalendarClientSecret string `json:"calendar_client_secret"`
}

var configFile string

func main() {
	defer log.Println("terminating")
	flag.Parse()
	logFile, err := os.OpenFile("log.txt", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer logFile.Close()
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)

	config, err := readConfig()
	if err != nil {
		log.Fatal(err)
	}

	bot := newBot(config)
	if err = bot.setAddress(config.Address); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	b, err := ioutil.ReadFile("client_secret.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	// If modifying these scopes, delete your previously saved credentials
	// at ~/.credentials/youtube-go-quickstart.json
	ytconfig, err := google.ConfigFromJSON(b, youtube.YoutubeReadonlyScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	bot.client = getClient(ctx, ytconfig)
	ytServ, err := youtube.New(bot.client)
	handleError(err, "Error creating YouTube client")
	bot.ytServ = ytServ
	res, err := bot.ytServ.Search.List("id,snippet").Q("YEE neva lie").MaxResults(5).Type("video").VideoCategoryId("10").Do()
	for _, item := range res.Items {
		fmt.Println(item.Snippet.Title)
		fmt.Printf("https://www.youtube.com/watch?v=%s\n", item.Id.VideoId)
		fmt.Println("----------------------------------------------------")
	}

	for {
		bot.lastPublic = time.Now().AddDate(0, 0, -1)
		log.Println("trying to establish connection")
		err = bot.connect()
		if err != nil {
			log.Println(err)
		}
		err = bot.close()
		if err != nil {
			log.Println(err)
		}
		time.Sleep(time.Second * 5)
	}
}

func readConfig() (*config, error) {
	file, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	bv, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, err
	}

	var c *config
	c = new(config)

	err = json.Unmarshal(bv, &c)
	if err != nil {
		return nil, err
	}

	return c, err
}

func newBot(config *config) *bot {
	return &bot{authToken: ";jwt=" + config.AuthToken}
}

func (b *bot) setAddress(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if url == "" {
		return errors.New("url address not supplied")
	}

	b.address = url
	return nil
}

func (b *bot) connect() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	header := http.Header{}
	header.Add("Cookie", fmt.Sprintf("authtoken=%s", b.authToken))

	conn, resp, err := websocket.DefaultDialer.Dial(b.address, header)
	if err != nil {
		return fmt.Errorf("handshake failed with status: %v", resp)
	}
	log.Println("Connection established.")

	b.conn = conn

	err = b.listen()
	if err != nil {
		return err
	}
	return nil
}

func (b *bot) listen() error {
	errc := make(chan error)
	go func() {
		for {
			_, message, err := b.conn.ReadMessage()
			if err != nil {
				errc <- fmt.Errorf("error trying to read message: %v", err)
				return
			}
			m, err := parseMessage(message)
			if err != nil {
				log.Println(err)
				continue
			}

			if m.Contents != nil {
				if m.Type == "PRIVMSG" {
					go func() {
						err := b.answer(m.Contents, true)
						if err != nil {
							errc <- err
							return
						}
					}()
				} else if strings.Contains(m.Contents.Data, "whenis") {
					go func() {
						err := b.answer(m.Contents, false)
						if err != nil {
							errc <- err
							return
						}
					}()
				}
			}
		}
	}()
	err := <-errc
	return err
}

func (b *bot) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.conn == nil {
		return errors.New("connection already closed")
	}

	err := b.conn.Close()
	if err != nil {
		return fmt.Errorf("error trying to close connection: %v", err)
	}

	b.conn = nil
	return nil
}

func (b *bot) answer(contents *contents, private bool) error {
	return nil
}

func parseMessage(msg []byte) (*message, error) {
	received := string(msg)
	m := new(message)
	maxBound := strings.IndexByte(received, ' ')
	if maxBound < 0 {
		return nil, errors.New("couldn't parse message type")
	}
	m.Type = received[:maxBound]
	m.Contents = parseContents(received, len(m.Type))
	return m, nil
}

func parseContents(received string, length int) *contents {
	contents := contents{}
	json.Unmarshal([]byte(received[length:]), &contents)
	return &contents
}

func init() {
	flag.StringVar(&configFile, "config", "config.json", "location of config")
}

func (b *bot) multiSendMsg(messages []string, nick string) error {
	for _, message := range messages {
		err := b.sendMsg(message, nick)
		if err != nil {
			return err
		}
		time.Sleep(time.Millisecond * 500)
	}
	return nil
}

func (b *bot) sendMsg(message string, nick string) error {
	cont := contents{Nick: nick, Data: message}
	messageS, _ := json.Marshal(cont)
	log.Printf("sending private response: %q", message)
	// TODO: properly marshal message as json
	err := b.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`PRIVMSG %s`, messageS)))
	if err != nil {
		log.Printf(err.Error())
	}
	return err
}

func (b *bot) replyHelp(nick string) error {
	responses := []string{
		"placeholder",
		"placeholder 2",
	}
	return b.multiSendMsg(responses, nick)
}
