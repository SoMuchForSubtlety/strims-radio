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
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

type bot struct {
	mu             sync.Mutex
	authToken      string
	address        string
	conn           *websocket.Conn
	client         *http.Client
	ytServ         *youtube.Service
	waitingQueue   queue
	openDecisions  decisions
	currentEntry   queueEntry
	con            config
	runningCommand *exec.Cmd
	skipVotes      int
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
	AuthToken string `json:"auth_token"`
	Address   string `json:"address"`
	Ingest    string `json:"ingest"`
	Key       string `json:"key"`
	APIKey    string `json:"api_key"`
}

type video struct {
	Title string
	ID    string
}

type queueEntry struct {
	Video video
	User  string
}

type queue struct {
	Items []queueEntry
	sync.Mutex
}

type decisions struct {
	requests map[string][]video
	sync.Mutex
}

const youtubeURLStart = "https://www.youtube.com/watch?v="

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
		log.Fatalf("error reading config file: %v", err)
	}

	bot := newBot(*config)
	if err = bot.setAddress(config.Address); err != nil {
		log.Fatal(err)
	}
	bot.openDecisions.requests = make(map[string][]video)

	bot.client = &http.Client{Transport: &transport.APIKey{Key: config.APIKey}}
	ytServ, err := youtube.New(bot.client)
	handleError(err, "Error creating YouTube client")
	bot.ytServ = ytServ

	go bot.play()

	for {
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

func newBot(config config) *bot {
	return &bot{authToken: ";jwt=" + config.AuthToken, con: config}
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
	urlType1 := regexp.MustCompile(`https:\/\/www.youtube.com\/watch\?v=[a-zA-Z0-9_-]+`)
	urlType2 := regexp.MustCompile(`https:\/\/youtu.be\/[a-zA-Z0-9_-]+`)
	var videoID string

	if i, err := strconv.Atoi(contents.Data); err == nil {
		return b.selectVideo(i, contents.Nick)
	} else if contents.Data == "-playing" {
		return b.sendMsg(fmt.Sprintf("curretly playing: %q requested by %s", b.currentEntry.Video.Title, b.currentEntry.User), contents.Nick)
	} else if contents.Data == "-next" {
		next, err := b.waitingQueue.peek()
		if err != nil {
			return b.sendMsg("No song queued", contents.Nick)
		}
		return b.sendMsg(fmt.Sprintf("up next: %q requested by %s", next.Video.Title, next.User), contents.Nick)
	} else if contents.Data == "-skiplkjhsdefcdlkjhasdfljhb" {
		b.skipVotes++
		if contents.Nick == "SoMuchForSubtlety" {
			b.skipVotes += 100
		}
		if b.skipVotes > len(b.waitingQueue.Items) {
			if b.runningCommand != nil {
				b.runningCommand.Process.Kill()
				b.skipVotes = 0
			}
		}
		return b.sendMsg(fmt.Sprintf("%v/%v votes to skip", b.skipVotes, len(b.waitingQueue.Items)/2), contents.Nick)
	} else if contents.Data == "-queue" {
		s1 := fmt.Sprintf("There are currently %v songs in the queue", len(b.waitingQueue.Items))
		b.sendMsg(s1, contents.Nick)
	} else if urlType1.Match([]byte(contents.Data)) {
		videoID = regexp.MustCompile(`v=[a-zA-Z0-9_-]+`).FindString(contents.Data)[2:]
	} else if urlType2.Match([]byte(contents.Data)) {
		videoID = regexp.MustCompile(`.be\/[a-zA-Z0-9_-]+`).FindString(contents.Data)[4:]
	}

	if videoID == "" {
		return b.sendMsg("invalid url", contents.Nick)
	}

	res, err := b.ytServ.Videos.List("id,snippet,contentDetails").Id(videoID).Do()
	if err != nil {
		log.Printf("encoutered error: %v", err)
		return b.sendMsg("there was an error", contents.Nick)
	} else if len(res.Items) < 1 {
		return b.sendMsg("invalid url", contents.Nick)
	} else if duration, _ := time.ParseDuration(strings.ToLower(res.Items[0].ContentDetails.Duration[2:])); duration.Minutes() >= 10 {
		return b.sendMsg("This song is too long, please keep it under 10 minutes", contents.Nick)
	} else {
		fmt.Println(duration)
		fmt.Println(res.Items[0].ContentDetails.Duration)
	}

	v := video{Title: res.Items[0].Snippet.Title, ID: res.Items[0].Id}
	entry := queueEntry{Video: v, User: contents.Nick}
	return b.push(entry)
}

func (b *bot) selectVideo(i int, nick string) error {
	b.openDecisions.Lock()
	defer b.openDecisions.Unlock()
	videos := b.openDecisions.requests[nick]
	if videos == nil {
		return b.sendMsg("no open video selection", nick)
	} else if i > len(videos) || i < 0 {
		return b.sendMsg("selection out of range", nick)
	}
	b.openDecisions.requests[nick] = nil

	entry := queueEntry{Video: videos[i], User: nick}
	b.push(entry)
	return b.sendMsg(fmt.Sprintf("selected (%v) %q", i, videos[i].Title), nick)
}

func (b *bot) push(newEntry queueEntry) error {
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	for i, entry := range b.waitingQueue.Items {
		if entry.User == newEntry.User {
			b.waitingQueue.Items[i] = newEntry
			return b.sendMsg("Replaced your previous selection", newEntry.User)
		}
	}
	b.waitingQueue.Items = append(b.waitingQueue.Items, newEntry)
	return b.sendMsg("added your request to the queue", newEntry.User)
}

func (q *queue) pop() (queueEntry, error) {
	q.Lock()
	defer q.Unlock()
	if len(q.Items) < 1 {
		return queueEntry{}, errors.New("can't pop from empty queue")
	}
	entry := q.Items[0]
	q.Items = q.Items[1:]
	return entry, nil
}

func (q *queue) peek() (queueEntry, error) {
	q.Lock()
	defer q.Unlock()
	if len(q.Items) < 1 {
		return queueEntry{}, errors.New("can't pop from empty queue")
	}
	entry := q.Items[0]
	return entry, nil
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
	log.Printf("sending private response to [%v]: %q", nick, messageS)
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

func (b *bot) play() {
	for {
		entry, err := b.waitingQueue.pop()
		if err != nil {
			fmt.Println("nothing in the queue")
			// TODO: not ideal, maybe have a backup playlist
			time.Sleep(time.Second * 5)
			continue
		}
		b.currentEntry = entry
		b.sendMsg("Playing your song now", b.currentEntry.User)
		command := exec.Command("youtube-dl", "-f", "bestaudio", "-g", youtubeURLStart+b.currentEntry.Video.ID)
		url, err := command.Output()
		if err != nil {
			log.Printf("encountered error trying to get url from youtube-dl: %v", err)
			continue
		}
		urlProper := strings.TrimSpace(string(url))
		command = exec.Command("ffmpeg", "-reconnect", "1", "-reconnect_at_eof", "1", "-reconnect_delay_max", "3", "-re", "-i", urlProper, "-codec:a", "aac", "-f", "flv", b.con.Ingest+b.con.Key)
		log.Printf("Now Playing %s's request: %s", b.currentEntry.User, b.currentEntry.Video.Title)
		b.runningCommand = command
		err = command.Run()
		b.runningCommand = nil
		if err != nil {
			log.Printf("encountered error trying to play with ffmpeg: %v", err)
		}
	}
}
