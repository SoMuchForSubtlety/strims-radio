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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	currentEntry   queueEntry
	con            config
	runningCommand *exec.Cmd
	skipVotes      int
	msgSenderChan  chan contents
	errorChan      chan error
	songStarted    time.Time
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
	Title    string
	ID       string
	Duration time.Duration
}

type queueEntry struct {
	Video video
	User  string
}

type queue struct {
	Items []queueEntry
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
	bot.msgSenderChan = make(chan contents, 100)

	file, err := ioutil.ReadFile("queue.json")
	if err != nil {
		fmt.Printf("no previous playlist found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &bot.waitingQueue)
		if err != nil {
			fmt.Printf("error trying to unmarshal queue: %v", err)
		} else {
			log.Printf("loaded playlist with %v songs", len(bot.waitingQueue.Items))
		}
	}

	bot.client = &http.Client{Transport: &transport.APIKey{Key: config.APIKey}}
	ytServ, err := youtube.New(bot.client)
	if err != nil {
		log.Fatalf("error creating client: %v", err)
	}
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

	go b.messageSender()
	err = b.listen()
	if err != nil {
		return err
	}
	return nil
}

func (b *bot) listen() error {
	go func() {
		for {
			_, message, err := b.conn.ReadMessage()
			if err != nil {
				b.errorChan <- fmt.Errorf("error trying to read message: %v", err)
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
						b.answer(m.Contents, true)
					}()
				}
			}
		}
	}()
	err := <-b.errorChan
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

func (b *bot) answer(contents *contents, private bool) {
	urlType1 := regexp.MustCompile(`https:\/\/www.youtube.com\/watch\?v=[a-zA-Z0-9_-]+`)
	urlType2 := regexp.MustCompile(`https:\/\/youtu.be\/[a-zA-Z0-9_-]+`)
	var videoID string
	var duration time.Duration

	if contents.Data == "-playing" {
		elapsed := time.Since(b.songStarted)

		response := fmt.Sprintf("`%v` `%v/%v` curretly playing: %q requested by %s", durationBar(15, elapsed, b.currentEntry.Video.Duration), fmtDuration(elapsed), fmtDuration(b.currentEntry.Video.Duration), b.currentEntry.Video.Title, b.currentEntry.User)

		b.sendMsg(response, contents.Nick)
		return
	} else if contents.Data == "-next" {
		next, err := b.waitingQueue.peek()
		if err != nil {
			b.sendMsg("No song queued", contents.Nick)
			return
		}
		b.sendMsg(fmt.Sprintf("up next: %q requested by %s", next.Video.Title, next.User), contents.Nick)
		return
	} else if contents.Data == "-skiplkjhsdefcdlkjhasdfljhb" {
		b.skipVotes++
		if b.skipVotes > len(b.waitingQueue.Items) {
			if b.runningCommand != nil {
				b.runningCommand.Process.Kill()
			}
			b.skipVotes = 0
		}
		b.sendMsg(fmt.Sprintf("%v/%v votes to skip", b.skipVotes, len(b.waitingQueue.Items)), contents.Nick)
		return
	} else if contents.Data == "-queue" {
		s1 := fmt.Sprintf("There are currently %v songs in the queue", len(b.waitingQueue.Items))
		usepos := b.getUserPosition(contents.Nick)
		if usepos >= 0 {
			s1 += fmt.Sprintf(", you are at position %v", usepos+1)
			d, err := b.durationUntilUser(contents.Nick)
			if err == nil {
				s1 += fmt.Sprintf(" and your song will play in %v", fmtDuration(d))
			}
		}

		b.sendMsg(s1, contents.Nick)
		return
	} else if urlType1.Match([]byte(contents.Data)) {
		videoID = regexp.MustCompile(`v=[a-zA-Z0-9_-]+`).FindString(contents.Data)[2:]
	} else if urlType2.Match([]byte(contents.Data)) {
		videoID = regexp.MustCompile(`.be\/[a-zA-Z0-9_-]+`).FindString(contents.Data)[4:]
	}

	if videoID == "" {
		b.sendMsg("invalid url", contents.Nick)
		return
	}

	res, err := b.ytServ.Videos.List("id,snippet,contentDetails").Id(videoID).Do()
	if err != nil {
		log.Printf("encoutered error: %v", err)
		b.sendMsg("there was an error", contents.Nick)
		return
	} else if len(res.Items) < 1 {
		b.sendMsg("invalid url", contents.Nick)
		return
	} else if duration, _ = time.ParseDuration(strings.ToLower(res.Items[0].ContentDetails.Duration[2:])); duration.Minutes() >= 10 {
		b.sendMsg("This song is too long, please keep it under 10 minutes", contents.Nick)
		return
	}

	v := video{Title: res.Items[0].Snippet.Title, ID: res.Items[0].Id, Duration: duration}
	entry := queueEntry{Video: v, User: contents.Nick}
	b.push(entry)
}

func (b *bot) push(newEntry queueEntry) {
	log.Printf("adding '%s' for [%v]", newEntry.Video.Title, newEntry.User)
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	for i, entry := range b.waitingQueue.Items {
		if entry.User == newEntry.User {
			b.waitingQueue.Items[i] = newEntry
			b.sendMsg("Replaced your previous selection", newEntry.User)
			return
		}
	}
	b.waitingQueue.Items = append(b.waitingQueue.Items, newEntry)
	b.sendMsg("added your request to the queue", newEntry.User)
	b.savePlaylist()
	return
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

func (b *bot) play() {
	for {
		entry, err := b.waitingQueue.pop()
		if err != nil {
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
		b.songStarted = time.Now()
		command = exec.Command("ffmpeg", "-reconnect", "1", "-reconnect_at_eof", "1", "-reconnect_delay_max", "3", "-re", "-i", urlProper, "-codec:a", "aac", "-f", "flv", b.con.Ingest+b.con.Key)
		log.Printf("Now Playing %s's request: %s", b.currentEntry.User, b.currentEntry.Video.Title)
		b.runningCommand = command
		err = command.Start()
		if err != nil {
			log.Printf("encountered error trying to play with ffmpeg: %v", err)
		}
		err = command.Wait()
		if err != nil {
			log.Printf("Command aborted or errored %v", err)
		}
		b.savePlaylist()
		b.runningCommand = nil
	}
}

func (b *bot) sendMsg(message string, nick string) {
	b.msgSenderChan <- contents{Nick: nick, Data: message}
}

func (b *bot) messageSender() {
	for {
		cont := <-b.msgSenderChan
		messageS, _ := json.Marshal(cont)
		log.Printf("sending private response to [%v]: '%s'", cont.Nick, cont.Data)
		// TODO: properly marshal message as json
		err := b.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`PRIVMSG %s`, messageS)))
		if err != nil {
			log.Printf(err.Error())
			b.errorChan <- err
			return
		}
		time.Sleep(time.Millisecond * 350)
	}
}

func (b *bot) getUserPosition(nick string) int {
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	for i, content := range b.waitingQueue.Items {
		if content.User == nick {
			return i
		}
	}
	return -1
}

func (b *bot) durationUntilUser(nick string) (time.Duration, error) {
	var dur time.Duration
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	for _, content := range b.waitingQueue.Items {
		if content.User != nick {
			dur += content.Video.Duration
		} else {
			currentRemaining := b.currentEntry.Video.Duration - time.Since(b.songStarted)
			return dur + currentRemaining, nil
		}
	}
	return dur, errors.New("user is not in queue")
}

func replaceAtIndex(in string, r rune, i int) string {
	out := ""
	j := 0
	for _, c := range in {
		if j != i {
			out += string(c)
		} else {
			out += string(r)
		}
		j++
	}
	return out
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	minutes := d / time.Minute
	d -= minutes * time.Minute
	seconds := d / time.Second
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func durationBar(width int, fraction time.Duration, total time.Duration) string {
	base := strings.Repeat("—", width)
	pos := float64(fraction.Seconds() / total.Seconds())

	newpos := int(float64(utf8.RuneCountInString(base))*pos) - 1
	if newpos < 0 {
		newpos = 0
	} else if newpos >= utf8.RuneCountInString(base) {
		newpos = utf8.RuneCountInString(base) - 1
	}

	return replaceAtIndex(base, '⚫', newpos)
}

func (b *bot) savePlaylist() error {
	file, err := json.MarshalIndent(&b.waitingQueue, "", "	")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile("queue.json", file, 0644)
	if err != nil {
		return err
	}
	return nil
}
