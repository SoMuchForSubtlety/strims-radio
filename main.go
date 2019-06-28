package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MemeLabs/dggchat"
	"github.com/SoMuchForSubtlety/fileupload"
	"github.com/SoMuchForSubtlety/opendj"
	"github.com/Syfaro/haste-client"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

type config struct {
	AuthToken  string   `json:"auth_token"`
	Address    string   `json:"address"`
	Rtmp       string   `json:"rtmp"`
	APIKey     string   `json:"api_key"`
	Moderators []string `json:"moderators"`
}

type localQueue struct {
	q []opendj.QueueEntry
}

type controller struct {
	ytServ    *youtube.Service
	cfg       config
	sgg       *dggchat.Session
	msgBuffer chan outgoingMessage
	dj        opendj.Dj

	haste *haste.Haste

	playlistLink  string
	playlistDirty bool

	likes             userList
	updateSubscribers userList
}

type outgoingMessage struct {
	nick    string
	message string
}

func main() {
	var err error
	var cont controller
	cont.cfg, err = readConfig("config.json")
	if err != nil {
		log.Fatalf("[ERROR] could not load config: %v", err)
	}

	client := &http.Client{Transport: &transport.APIKey{Key: cont.cfg.APIKey}}
	cont.ytServ, err = youtube.New(client)
	if err != nil {
		log.Fatalf("[ERROR] could not connect to youtube API: %v", err)
	}

	var queue localQueue

	file, err := ioutil.ReadFile("queue.json")
	if err != nil {
		log.Printf("[INFO] no previous playlist found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &queue)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal queue: %v", err)
		} else {
			log.Printf("[INFO] loaded playlist with %v songs", len(queue.q))
		}
	}

	dj, err := opendj.NewDj(queue.q)
	if err != nil {
		log.Fatalf("Failed to initialize dj: %v", err)
	}
	dj.AddNewSongHandler(cont.newSong)
	dj.Play(cont.cfg.Rtmp)

	// Create a new client
	cont.sgg, err = dggchat.New(cont.cfg.AuthToken)
	if err != nil {
		log.Fatalln(err)
	}

	// Open a connection
	err = cont.sgg.Open()
	if err != nil {
		log.Fatalln(err)
	}

	// Cleanly close the connection
	defer cont.sgg.Close()

	cont.sgg.AddPMHandler(cont.onPrivMessage)
	cont.sgg.AddErrorHandler(onError)

	// Wait for ctr-C to shut down
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT)
	<-sc

}

func readConfig(title string) (cfg config, err error) {
	file, err := os.Open(title)
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	bv, err := ioutil.ReadAll(file)
	if err != nil {
		return cfg, err
	}

	err = json.Unmarshal(bv, &cfg)
	return cfg, err
}

func (c *controller) onPrivMessage(m dggchat.PrivateMessage, s *dggchat.Session) {
	log.Printf("New message from %s: %s\n", m.User, m.Message)

	trimmedMsg := strings.TrimSpace(m.Message)

	ytURL := regexp.MustCompile(`(youtube.com\/watch\?v=|youtu.be\/)[a-zA-Z0-9_-]+`)

	if ytURL.Match([]byte(trimmedMsg)) {
		c.addYTlink(m)
	}
	switch trimmedMsg {
	case "-playing":
		c.sendCurrentSong(m.User.Nick)
		return
	case "-next":
		c.sendNextSong(m.User.Nick)
		return
	case "-queue":
		c.sendQueuePositions(m.User.Nick)
		return
	case "-playlist":
		c.sendPlaylist(m.User.Nick)
		return
	default:
	}

	if strings.Contains(trimmedMsg, "-remove") {
		c.removeItem(trimmedMsg, m.User.Nick)
		return
	} else if strings.Contains(trimmedMsg, "-dedicate") {
		c.addDedication(m.Message, m.User.Nick)
	}

}

func onError(e string, s *dggchat.Session) {
	log.Printf("[ERROR] %s", e)
}

func (c *controller) addYTlink(m dggchat.PrivateMessage) {
	ytURLStart := "https://www.youtube.com/watch?v="

	id := regexp.MustCompile(`(\?v=|be\/)[a-zA-Z0-9-_]+`).FindString(m.Message)[3:]
	if id == "" {
		c.sendMsg("invalid link", m.User.Nick)
		return
	}

	res, err := c.ytServ.Videos.List("id,snippet,contentDetails").Id(id).Do()
	var duration time.Duration
	if err != nil {
		log.Printf("[ERROR] youtube API query failed: %v", err)
		c.sendMsg("there was an error", m.User.Nick)
		return
	} else if len(res.Items) < 1 {
		c.sendMsg("invalid link", m.User.Nick)
		return
	} else if duration, _ = time.ParseDuration(strings.ToLower(res.Items[0].ContentDetails.Duration[2:])); duration.Minutes() >= 10 {
		c.sendMsg("This song is too long, please keep it under 10 minutes", m.User.Nick)
		return
	}

	var video opendj.Media
	video.Title = res.Items[0].Snippet.Title
	video.Duration = duration
	video.URL = ytURLStart + res.Items[0].Id

	var entry opendj.QueueEntry
	entry.Media = video
	entry.Owner = m.User.Nick

	c.dj.AddEntry(entry)
}

func (c *controller) sendCurrentSong(nick string) {
	song, elapsed, err := c.dj.CurrentlyPlaying()
	video := song.Media
	if err != nil {
		c.sendMsg("there is nothing playing right now :(", nick)
		return
	}
	response := fmt.Sprintf("`%v` `%v/%v` currently playing: 🎶 %q 🎶 requested by %s", durationBar(15, elapsed, video.Duration), fmtDuration(elapsed), fmtDuration(video.Duration), video.Title, song.Owner)
	c.sendMsg(response, nick)
}

func (c *controller) sendNextSong(nick string) {
	queue := c.dj.Queue()
	if len(queue) <= 0 {
		c.sendMsg("there is nothing in the queue :(", nick)
		return
	}

	c.sendMsg(fmt.Sprintf("up next: '%v' requested by %s", queue[0].Media.Title, queue[0].Owner), nick)
}

func (c *controller) sendQueuePositions(nick string) {
	queue := c.dj.Queue()
	positions := c.dj.UserPosition(nick)
	durations := c.dj.DurationUntilUser(nick)
	response := fmt.Sprintf("There are currently %v songs in the queue", len(queue))
	if len(positions) > 0 {
		if len(positions) != len(durations) {
			c.sendMsg("there was an error", nick)
			return
		}
		for i, duration := range durations {
			response += fmt.Sprintf(", your song at position %v will play in %v", i, fmtDuration(duration))
		}
	}
	c.sendMsg(response, nick)
}

func (c *controller) sendPlaylist(nick string) {
	if c.playlistDirty {
		currentSong, _, _ := c.dj.CurrentlyPlaying()
		playlist := formatPlaylist(c.dj.Queue(), currentSong)

		url, err := c.uploadString(playlist)
		if err != nil {
			log.Printf("[ERROR] failed to upload playlist: %v", err)
			c.sendMsg("there was an error", nick)
			return
		}

		log.Println("[INFO] 📝 Generated playlist")
		c.playlistLink = url
		c.playlistDirty = false
	}

	c.sendMsg(fmt.Sprintf("you can find the current playlist here: %v", c.playlistLink), nick)
}

func (c *controller) removeItem(message string, nick string) {
	intString := strings.TrimSpace(strings.Replace(message, "-remove", "", -1))
	index, err := strconv.Atoi(intString)
	if err != nil {
		c.sendMsg("please enter a valid integer", nick)
		return
	}

	entry, err := c.dj.EntryAtIndex(index)
	if err != nil {
		c.sendMsg("Index out of range", nick)
		return
	}

	if nick != entry.Owner || !c.isMod(nick) {
		c.sendMsg(fmt.Sprintf("I can't allow you to do that, %v", nick), nick)
		return
	}

	err = c.dj.RemoveIndex(index)
	if err != nil {
		c.sendMsg("index out of range", nick)
		return
	}
	c.sendMsg("Successfully removed item at index", nick)
	return
}

func (c *controller) addDedication(message string, nick string) {
	positions := c.dj.UserPosition(nick)
	if len(positions) <= 0 {
		c.sendMsg("you have no songs in the queue", nick)
		return
	}

	dedication := strings.TrimSpace(strings.Replace(message, "-dedicate", "", -1))

	entry, err := c.dj.EntryAtIndex(positions[0])
	if err != nil {
		c.sendMsg("there was an error", nick)
		return
	}
	entry.Dedication = dedication
	err = c.dj.ChangeIndex(entry, positions[0])
	if err != nil {
		c.sendMsg("there was an error", nick)
		return
	}

	c.sendMsg(fmt.Sprintf("Dedicated %v to %v", entry.Media.Title, dedication), nick)
}

func (c *controller) isMod(nick string) bool {
	for _, mod := range c.cfg.Moderators {
		if nick == mod {
			return true
		}
	}
	return false
}

func (c *controller) uploadString(text string) (url string, err error) {
	var file *os.File

	type resp struct {
		response *haste.Response
		er       error
	}

	c1 := make(chan resp)
	go func() {
		hasteResp, err := c.haste.UploadString(text)
		c1 <- resp{response: hasteResp, er: err}
	}()

	select {
	case res := <-c1:
		err = res.er
		url = "https://hastebin.com/raw/" + res.response.Key
	case <-time.After(1 * time.Second):
		// TODO: find a better way to do this
		err = ioutil.WriteFile("tmp.txt", []byte(text), 0644)
		if err != nil {
			return url, err
		}
		file, err = os.Open("tmp.txt")
		if err != nil {
			return url, err
		}
		url, err = fileupload.UploadToHost("https://uguu.se/api.php?d=upload-tool", file)
	}

	return url, err
}

func (c *controller) sendMsg(message string, nick string) {
	if _, inChat := c.sgg.GetUser(nick); inChat {
		c.msgBuffer <- outgoingMessage{nick: nick, message: message}
	}
}

func (c *controller) messageSender() {
	for {
		// TODO: verify the message was sent
		msg := <-c.msgBuffer
		c.sgg.SendPrivateMessage(msg.nick, msg.message)
		time.Sleep(time.Millisecond * 450)
	}
}

func (c *controller) newSong(entry opendj.QueueEntry) {
	msg := fmt.Sprintf("Now Playing %s's request: %s", entry.Owner, entry.Media.Title)
	log.Println("[INFO] ▶ " + msg)

	c.updateSubscribers.Lock()
	for _, user := range c.updateSubscribers.Users {
		c.sendMsg(msg, user)
	}
	c.updateSubscribers.Unlock()

	if entry.Dedication != "" {
		c.sendMsg(fmt.Sprintf("%s dedicated this song to you.", entry.Owner), entry.Dedication)
	}

	c.sendMsg("Playing your song now", entry.Owner)
}
