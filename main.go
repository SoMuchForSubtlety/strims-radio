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
	"unicode/utf8"

	"github.com/SoMuchForSubtlety/fileupload"
	"github.com/Syfaro/haste-client"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

type bot struct {
	mu               sync.Mutex
	authToken        string
	address          string
	conn             *websocket.Conn
	client           *http.Client
	ytServ           *youtube.Service
	waitingQueue     queue
	currentEntry     queueEntry
	con              config
	runningCommand   *exec.Cmd
	msgSenderChan    chan contents
	errorChan        chan error
	songStarted      time.Time
	skipUsers        userList
	updateUsers      userList
	likedUsers       userList
	haste            *haste.Haste
	playlistURL      string
	playlistURLDirty bool
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
	AuthToken  string   `json:"auth_token"`
	Address    string   `json:"address"`
	Ingest     string   `json:"ingest"`
	Key        string   `json:"key"`
	APIKey     string   `json:"api_key"`
	Moderators []string `json:"moderators"`
}

type video struct {
	Title    string
	ID       string
	Duration time.Duration
}

type queueEntry struct {
	Video      video
	User       string
	Dedication string
}

type queue struct {
	Items []queueEntry
	sync.Mutex
}

const youtubeURLStart = "https://www.youtube.com/watch?v="

var configFile string

func main() {
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

	go bot.play()

	for {
		log.Println("[INFO] 🌐 trying to establish connection")
		err = bot.connect()
		if err != nil {
			log.Printf("[ERROR] bot error: %v", err)
		}
		err = bot.close()
		if err != nil {
			log.Printf("[ERROR] closing bot failed: %v", err)
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
	h := haste.NewHaste("https://hastebin.com")
	bot := bot{authToken: ";jwt=" + config.AuthToken, con: config, haste: h}
	bot.errorChan = make(chan error)
	bot.msgSenderChan = make(chan contents, 100)
	bot.client = &http.Client{Transport: &transport.APIKey{Key: config.APIKey}}
	ytServ, err := youtube.New(bot.client)
	if err != nil {
		log.Fatalf("[ERROR] could not connect to youtube API: %v", err)
	}
	bot.ytServ = ytServ

	file, err := ioutil.ReadFile("queue.json")
	if err != nil {
		log.Printf("[INFO] no previous playlist found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &bot.waitingQueue)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal queue: %v", err)
		} else {
			log.Printf("[INFO] loaded playlist with %v songs", len(bot.waitingQueue.Items))
		}
	}

	file, err = ioutil.ReadFile("updateUsers.json")
	if err != nil {
		log.Printf("[INFO] no user list found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &bot.updateUsers)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal user list: %v", err)
		} else {
			log.Printf("[INFO] loaded user list with %v entries", len(bot.waitingQueue.Items))
		}
	}

	return &bot
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
	log.Println("[INFO] ✔️ Connection established.")
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
				log.Printf("[ERROR] parsing message failed: %v", err)
				continue
			}

			if m.Contents != nil {
				if m.Type == "PRIVMSG" {
					go func() {
						b.answer(m.Contents, true)
					}()
				} else if m.Type == "ERR" {
					log.Printf("[ERROR] Chat error: %v", m.Contents.Data)
				}
			}
		}
	}()
	return <-b.errorChan
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
	contents.Data = strings.TrimSpace(contents.Data)

	if contents.Data == "-playing" {
		elapsed := time.Since(b.songStarted)
		bonus := ""
		if b.currentEntry.Dedication != "" {
			bonus = fmt.Sprintf("- dedicated to %s -", b.currentEntry.Dedication)
		}
		response := fmt.Sprintf("`%v` `%v/%v` currently playing: 🎶 %q 🎶 requested by %s %s %v ", durationBar(15, elapsed, b.currentEntry.Video.Duration), fmtDuration(elapsed), fmtDuration(b.currentEntry.Video.Duration), b.currentEntry.Video.Title, b.currentEntry.User, bonus, youtubeURLStart+b.currentEntry.Video.ID)

		b.sendMsg(response, contents.Nick)
		return
	} else if contents.Data == "-next" {
		next, err := b.peek()
		if err != nil {
			b.sendMsg("No song queued", contents.Nick)
			return
		}
		bonus := ""
		if b.currentEntry.Dedication != "" {
			bonus = fmt.Sprintf(" and dedicated to %s", b.currentEntry.Dedication)
		}
		b.sendMsg(fmt.Sprintf("up next: '%v' requested by %s%s", next.Video.Title, next.User, bonus), contents.Nick)
		return
	} else if contents.Data == "-skip" && false {
		index := b.skipUsers.search(contents.Nick)
		if index > 0 {
			b.sendMsg("You alreadyu voted to skip", contents.Nick)
			return
		}
		b.skipUsers.add(contents.Nick)
		if len(b.skipUsers.Users) >= len(b.waitingQueue.Items) {
			if b.runningCommand != nil {
				// !!! WINDOWS ONLY !!!
				// TODO: add unix implementation
				err := b.runningCommand.Process.Signal(windows.SIGABRT)
				if err != nil {
					log.Printf("[ERROR] encountered an error while trying to kill ffmpeg: %v", err)
				}
				b.runningCommand = nil
			}
			b.sendMsg(fmt.Sprintf("%v/%v votes to skip - skipping song", len(b.skipUsers.Users), len(b.waitingQueue.Items)), contents.Nick)
			b.skipUsers.clear()
			return
		}
		b.sendMsg(fmt.Sprintf("%v/%v votes to skip", len(b.skipUsers.Users), len(b.waitingQueue.Items)), contents.Nick)
		return
	} else if contents.Data == "-queue" {
		s1 := b.queuePositionMessage(contents.Nick)

		b.sendMsg(s1, contents.Nick)
		return
	} else if contents.Data == "-playlist" {
		if b.playlistURLDirty {
			var url string
			var err error
			var file *os.File
			playlist := b.formatPlaylist()
			type resp struct {
				response *haste.Response
				er       error
			}
			c1 := make(chan resp)
			go func() {
				hasteResp, err := b.haste.UploadString(playlist)
				c1 <- resp{response: hasteResp, er: err}
			}()

			select {
			case res := <-c1:
				err = res.er
				url = "https://hastebin.com/raw/" + res.response.Key
			case <-time.After(1 * time.Second):
				err = ioutil.WriteFile("playlist.txt", []byte(playlist), 0644)
				if err != nil {
					break
				}
				file, err = os.Open("playlist.txt")
				if err != nil {
					break
				}
				url, err = fileupload.UploadToHost("https://uguu.se/api.php?d=upload-tool", file)
			}
			if err != nil {
				log.Printf("[ERROR] failed to upload playlist: %v", err)
				b.sendMsg("there was an error", contents.Nick)
				return
			}
			log.Println("[INFO] 📝 Generated playlist")
			b.playlistURLDirty = false
			b.playlistURL = url
		}
		b.sendMsg(fmt.Sprintf("you can find the current playlist here: %v", b.playlistURL), contents.Nick)
		return
	} else if contents.Data == "-updateme" {
		index := b.updateUsers.search(contents.Nick)
		if index < 0 {
			b.updateUsers.add(contents.Nick)
			b.sendMsg("You will now get a message every time a new song plays. send `-updateme` again to turn it off.", contents.Nick)
		} else {
			b.updateUsers.remove(contents.Nick)
			b.sendMsg("You will no longer get notifications.", contents.Nick)
		}
		err := saveStruct(b.updateUsers, "updateUsers.json")
		if err != nil {
			log.Printf("[ERROR] failed to save struct: %v", err)
		}
		return
	} else if contents.Data == "-like" {
		b.likedUsers.add(contents.Nick)
		b.sendMsg(fmt.Sprintf("I will tell %v you like their song PeepoHappy ", b.currentEntry.User), contents.Nick)
		log.Printf("[INFO] 💖 %v liked %v's song", contents.Nick, b.currentEntry.User)
		return
	} else if strings.Contains(contents.Data, "-dedicate") {
		dedication := strings.TrimSpace(strings.Replace(contents.Data, "-dedicate", "", -1))
		err := b.addDedication(contents.Nick, dedication)
		if err != nil {
			b.sendMsg("You don't have a song in the queue.", contents.Nick)
			return
		} else if dedication == "" {
			b.sendMsg("dedicate deez nuts", contents.Nick)
			return
		}

		b.sendMsg(fmt.Sprintf("Dedicated your song to %s", dedication), contents.Nick)
		return
	} else if strings.Contains(contents.Data, "-remove") {
		if !b.checkMod(contents.Nick) {
			b.sendMsg("You're not mod", contents.Nick)
			return
		}
		intString := strings.TrimSpace(strings.Replace(contents.Data, "-remove", "", -1))
		index, err := strconv.Atoi(intString)
		if err != nil {
			b.sendMsg("please enter a valid integer", contents.Nick)
			return
		}
		err = b.removeIndex(index - 1)
		if err != nil {
			b.sendMsg("index out of range", contents.Nick)
			return
		}
		b.sendMsg("Successfully removed", contents.Nick)
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
		log.Printf("[ERROR] youtube API query failed: %v", err)
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
	b.playlistURLDirty = true
	b.waitingQueue.Lock()
	if len(b.waitingQueue.Items) > 5 {
		for i, entry := range b.waitingQueue.Items {
			if entry.User == newEntry.User {
				newEntry.Dedication = b.waitingQueue.Items[i].Dedication
				b.waitingQueue.Items[i] = newEntry
				b.waitingQueue.Unlock()
				log.Printf("[INFO] ♻️ changing %s's song to '%s'", newEntry.User, newEntry.Video.Title)
				b.sendMsg(fmt.Sprintf("Replaced your previous selection. %v", b.queuePositionMessage(newEntry.User)), newEntry.User)
				return
			}
		}
	}
	log.Printf("[INFO] ➕ adding '%s' for %s", newEntry.Video.Title, newEntry.User)
	b.waitingQueue.Items = append(b.waitingQueue.Items, newEntry)
	b.waitingQueue.Unlock()
	err := saveStruct(b.waitingQueue, "queue.json")
	if err != nil {
		log.Printf("[ERROR] failed to save struct: %v", err)
	}
	b.sendMsg(fmt.Sprintf("Added your request to the queue. %v", b.queuePositionMessage(newEntry.User)), newEntry.User)
}

func (b *bot) pop() (queueEntry, error) {
	b.playlistURLDirty = true
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	if len(b.waitingQueue.Items) < 1 {
		return queueEntry{}, errors.New("can't pop from empty queue")
	}
	entry := b.waitingQueue.Items[0]
	b.waitingQueue.Items = b.waitingQueue.Items[1:]
	return entry, nil
}

func (b *bot) peek() (queueEntry, error) {
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	if len(b.waitingQueue.Items) < 1 {
		return queueEntry{}, errors.New("can't pop from empty queue")
	}
	entry := b.waitingQueue.Items[0]
	return entry, nil
}

func (b *bot) removeIndex(index int) error {
	b.playlistURLDirty = true
	b.waitingQueue.Lock()
	if index >= len(b.waitingQueue.Items) || index < 0 {
		return errors.New("index out of range")
	}
	song := b.waitingQueue.Items[index]
	b.waitingQueue.Items = append(b.waitingQueue.Items[:index], b.waitingQueue.Items[index+1:]...)
	b.waitingQueue.Unlock()
	log.Printf("[INFO] 🗑️ removed %v's song '%v' at index %v", song.User, song.Video.Title, index)
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
	err := json.Unmarshal([]byte(received[length:]), &contents)
	if err != nil {
		contents.Nick = "strims"
		contents.Data = received
	}
	return &contents
}

func init() {
	flag.StringVar(&configFile, "config", "config.json", "location of config")
}

func (b *bot) play() {
	for {
		b.skipUsers.clear()
		entry, err := b.pop()
		if err != nil {
			// TODO: not ideal, maybe have a backup playlist
			time.Sleep(time.Second * 5)
			continue
		}
		b.currentEntry = entry

		b.sendMsg("Playing your song now", b.currentEntry.User)

		for _, user := range b.updateUsers.Users {
			bonus := ""
			if b.currentEntry.Dedication != "" {
				bonus = fmt.Sprintf("- dedicated to %s", b.currentEntry.Dedication)
			}
			b.sendMsg(fmt.Sprintf("Now Playing %s's request: %s %s", b.currentEntry.User, b.currentEntry.Video.Title, bonus), user)
		}

		if b.currentEntry.Dedication != "" {
			b.sendMsg(fmt.Sprintf("%s dedicated this song to you 💖", b.currentEntry.User), b.currentEntry.Dedication)
		}

		command := exec.Command("youtube-dl", "-f", "bestaudio", "-g", youtubeURLStart+b.currentEntry.Video.ID)
		url, err := command.Output()
		if err != nil {
			log.Printf("[ERROR] couldn't get url from youtube-dl: %v", err)
			continue
		}

		urlProper := strings.TrimSpace(string(url))
		b.songStarted = time.Now()
		command = exec.Command("ffmpeg", "-reconnect", "1", "-reconnect_at_eof", "1", "-reconnect_delay_max", "3", "-re", "-i", urlProper, "-codec:a", "aac", "-f", "flv", b.con.Ingest+b.con.Key)
		log.Printf("[INFO] ▶ Now Playing %s's request: %s", b.currentEntry.User, b.currentEntry.Video.Title)
		b.runningCommand = command
		err = command.Start()
		if err != nil {
			log.Printf("[ERROR] failed to start ffmpeg: %v", err)
		}
		err = command.Wait()
		if err != nil {
			log.Printf("[ERROR] ffmpeg aborted or errored: %v", err)
		}
		log.Println("[INFO] 🛑 Done Playing")
		if len(b.likedUsers.Users) > 0 {
			ppl := "people"
			if len(b.likedUsers.Users) == 1 {
				ppl = "person"
			}
			b.sendMsg(fmt.Sprintf("%v %v really liked your song PeepoHappy", len(b.likedUsers.Users), ppl), b.currentEntry.User)
		}
		b.likedUsers.clear()
		err = saveStruct(b.waitingQueue, "queue.json")
		if err != nil {
			log.Printf("[ERROR] failed to save struct: %v", err)
		}
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
		log.Printf("[MSG] sending message to %v: '%s'", cont.Nick, cont.Data)
		err := b.conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`PRIVMSG %s`, messageS)))
		if err != nil {
			b.errorChan <- err
			return
		}
		time.Sleep(time.Millisecond * 400)
	}
}

func (b *bot) getUserPosition(nick string) int {
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

func saveStruct(v interface{}, title string) error {
	file, err := json.MarshalIndent(&v, "", "	")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(title, file, 0644)
	if err != nil {
		return err
	}
	return nil
}

func (b *bot) queuePositionMessage(nick string) string {
	s1 := fmt.Sprintf("There are currently %v songs in the queue", len(b.waitingQueue.Items))
	usepos := b.getUserPosition(nick)
	if usepos >= 0 {
		s1 += fmt.Sprintf(", you are at position %v", usepos+1)
		d, err := b.durationUntilUser(nick)
		if err == nil {
			s1 += fmt.Sprintf(" and your song will play in %v", fmtDuration(d))
		}
	}
	return s1
}

func (b *bot) addDedication(owner string, target string) error {
	b.waitingQueue.Lock()
	defer b.waitingQueue.Unlock()
	position := b.getUserPosition(owner)
	if position < 0 {
		return errors.New("user not in queue")
	}
	b.waitingQueue.Items[position].Dedication = target
	return nil
}

func (b *bot) formatPlaylist() string {
	lines := fmt.Sprintf(" curretly playing: 🎶 %q 🎶 requested by %s\n\n", b.currentEntry.Video.Title, b.currentEntry.User)
	var maxname int
	for _, vid := range b.waitingQueue.Items {
		if len(vid.User) > maxname {
			maxname = len(vid.User)
		}
	}
	for i, vid := range b.waitingQueue.Items {
		lines += fmt.Sprintf(" %2v | %-*v | %v | %v\n", i+1, maxname, vid.User, fmtDuration(vid.Video.Duration), vid.Video.Title)
	}
	return lines
}

func (b *bot) checkMod(nick string) bool {
	for _, mod := range b.con.Moderators {
		if nick == mod {
			return true
		}
	}
	return false
}
