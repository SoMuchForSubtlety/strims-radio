package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
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

type controller struct {
	ytServ    *youtube.Service
	cfg       config
	sgg       *dggchat.Session
	msgBuffer chan outgoingMessage
	dj        *opendj.Dj

	haste *haste.Haste

	playlistLink  string
	playlistDirty bool

	likes             userList
	updateSubscribers userList

	backupSongs       []opendj.QueueEntry
	recentBackupSongs []int
}

type outgoingMessage struct {
	nick    string
	message string
}

var (
	configLocation         = flag.String("config", "config.json", "the location of the config file")
	queueSaveLocation      = flag.String("queue", "queue.json", "the location of the saved queue")
	backupSongsLocation    = flag.String("songs", "songs.json", "the location of the backup songs")
	subscriberSaveLocation = flag.String("users", "updateUsers.json", "the location of the list of users that want to get notifications")
)

const (
	hasteURL   = "https://hastebin.com"
	ytURLStart = "https://www.youtube.com/watch?v="
	uguuURL    = "https://uguu.se/api.php?d=upload-tool"
	tmpFile    = "tmp.txt"
)

func main() {
	flag.Parse()
	cont, err := initController()
	if err != nil {
		log.Fatalf("[ERROR] could not initialize controller: %v", err)
	}

	// Open a connection
	err = cont.sgg.Open()
	if err != nil {
		log.Fatalln(err)
	}
	// Cleanly close the connection
	defer cont.sgg.Close()

	if len(cont.backupSongs) > 0 && len(cont.dj.Queue()) <= 0 {
		cont.dj.AddEntry(cont.backupSongs[rand.Intn(len(cont.backupSongs))])
	}

	go cont.dj.Play(cont.cfg.Rtmp)

	cont.messageSender()
	// Wait for ctr-C to shut down
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT)
	<-sc
}

func initController() (c *controller, err error) {
	c = &controller{}
	c.playlistDirty = true
	c.recentBackupSongs = make([]int, 5)

	c.cfg, err = readConfig(*configLocation)
	if err != nil {
		return nil, err
	}

	c.msgBuffer = make(chan outgoingMessage, 100)
	c.haste = haste.NewHaste(hasteURL)

	client := &http.Client{Transport: &transport.APIKey{Key: c.cfg.APIKey}}
	c.ytServ, err = youtube.New(client)
	if err != nil {
		return nil, err
	}

	var queue []opendj.QueueEntry
	// load the saved playlist if there is one
	file, err := ioutil.ReadFile(*queueSaveLocation)
	if err != nil {
		log.Printf("[INFO] no previous playlist found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &queue)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal queue: %v", err)
		} else {
			log.Printf("[INFO] loaded playlist with %v songs", len(queue))
		}
	}

	// load update subscribers
	file, err = ioutil.ReadFile(*subscriberSaveLocation)
	if err != nil {
		log.Printf("[INFO] no user list found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &c.updateSubscribers)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal user list: %v", err)
		} else {
			log.Printf("[INFO] loaded user list with %v entries", len(c.updateSubscribers.Users))
		}
	}

	// load backup songs
	file, err = ioutil.ReadFile(*backupSongsLocation)
	if err != nil {
		log.Printf("[INFO] no backup song list found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &c.backupSongs)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal backup songs list: %v", err)
		} else {
			log.Printf("[INFO] loaded backup songs with %v entries", len(c.backupSongs))
		}
	}

	// create dj
	c.dj = opendj.NewDj(queue)

	c.dj.AddNewSongHandler(c.newSong)
	c.dj.AddEndOfSongHandler(c.songOver)
	c.dj.AddPlaybackErrorHandler(c.songError)

	// Create a new sgg client
	c.sgg, err = dggchat.New(";jwt=" + c.cfg.AuthToken)
	u, err := url.Parse(c.cfg.Address)
	if err != nil {
		log.Fatalf("[ERROR] can't parse url %v", err)
	}
	c.sgg.SetURL(*u)

	if err != nil {
		return nil, err
	}

	c.sgg.AddPMHandler(c.onPrivMessage)
	c.sgg.AddErrorHandler(onError)

	return c, nil
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
	case "-updateme":
		c.addUserToUpdates(m.User.Nick)
		return
	case "-like":
		c.likeSong(m.User.Nick)
		return
	default:
	}

	if strings.Contains(trimmedMsg, "-remove") {
		c.removeItem(trimmedMsg, m.User)
		return
	} else if strings.Contains(trimmedMsg, "-dedicate") {
		c.addDedication(m.Message, m.User.Nick)
		return
	} else if strings.Contains(trimmedMsg, "-addbackup") {
		c.addSongToBackup(trimmedMsg, m.User.Nick)
		return
	} else if ytURL.Match([]byte(trimmedMsg)) {
		c.addYTlink(m)
		return
	}

	c.sendMsg("unknown command", m.User.Nick)
}

func onError(e string, s *dggchat.Session) {
	log.Printf("[ERROR] error from ws: %s", e)
}

func (c *controller) addYTlink(m dggchat.PrivateMessage) {
	queue := c.dj.Queue()
	var duration time.Duration
	var maxduration float64

	for _, item := range queue {
		duration += item.Media.Duration
	}

	userPositions := c.dj.UserPosition(m.User.Nick)
	var userTotalDuration time.Duration
	for _, j := range userPositions {
		userTotalDuration += queue[j].Media.Duration
	}

	if userTotalDuration.Minutes() > 30 {
		c.sendMsg("You already have over 30 minutes queued, please wait a while before you add more.", m.User.Nick)
		return
	}

	item, progress, err := c.dj.CurrentlyPlaying()
	if err == nil {
		duration += item.Media.Duration - progress
	}

	if duration.Minutes() <= 1 {
		maxduration = 60
	} else if duration.Minutes() <= 15 {
		maxduration = 20
	} else if duration.Minutes() <= 60 {
		maxduration = 10
	} else {
		maxduration = 5
	}

	id, err := ytIDfromURL(m.Message)
	if err != nil {
		c.sendMsg("invalid link", m.User.Nick)
		return
	}

	entry, err := c.createYTQueueEntry(id, m.User.Nick)
	if err != nil {
		log.Printf("[ERROR] couldn't get song: %v", err)
		c.sendMsg("not found", m.User.Nick)
		return
	} else if entry.Media.Duration.Minutes() >= maxduration {
		c.sendMsg(fmt.Sprintf("This song is too long, please keep it under %v minutes", maxduration), m.User.Nick)
		return
	}

	c.dj.AddEntry(entry)
	saveStruct(c.dj.Queue(), *queueSaveLocation)
	c.playlistDirty = true

	durations := c.dj.DurationUntilUser(m.User.Nick)
	positions := c.dj.UserPosition(m.User.Nick)
	response := ""
	if len(positions) > 0 {
		if len(positions) != len(durations) {
			log.Printf("[ERROR] duration and position length mismatch")
		} else {
			response = fmt.Sprintf("It is in position %v and will play in %v", positions[len(positions)-1]+1, fmtDuration(durations[len(durations)-1]))
		}
	}

	c.sendMsg(fmt.Sprintf("Added your request '%v' to the queue. %v", entry.Media.Title, response), m.User.Nick)
	log.Printf("Added song: '%v' for %v", entry.Media.Title, entry.Owner)
}

func (c *controller) sendCurrentSong(nick string) {
	song, elapsed, err := c.dj.CurrentlyPlaying()
	video := song.Media
	if err != nil {
		c.sendMsg("there is nothing playing right now :(", nick)
		return
	}
	response := fmt.Sprintf("`%v` `%v/%v` currently playing: 🎶 \"%s\" 🎶 requested by %s", durationBar(15, elapsed, video.Duration), fmtDuration(elapsed), fmtDuration(video.Duration), video.Title, song.Owner)
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
		response += fmt.Sprintf(", your next song is in position %v and will play in %v", positions[0]+1, fmtDuration(durations[0]))
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

func (c *controller) likeSong(nick string) {
	playing, _, err := c.dj.CurrentlyPlaying()
	if playing.Owner == nick {
		c.sendMsg("You can't like your own song PepoBan", nick)
		return
	}
	if err != nil {
		c.sendMsg("There is nothing currently playing.", nick)
		return
	}
	if c.likes.search(nick) >= 0 {
		c.sendMsg("You already liked this song.", nick)
		return
	}
	c.likes.add(nick)
	c.sendMsg(fmt.Sprintf("I'll tell %v you liked %v PeepoHappy", playing.Owner, playing.Media.Title), nick)
}

func (c *controller) addUserToUpdates(nick string) {
	index := c.updateSubscribers.search(nick)
	if index < 0 {
		c.sendMsg("You will now get a message every time a new song plays. send `-updateme` again to turn it off.", nick)
		c.updateSubscribers.add(nick)
	} else {
		c.sendMsg("You will no longer get notifications.", nick)
		c.updateSubscribers.remove(nick)
	}
	saveStruct(&c.updateSubscribers, *subscriberSaveLocation)
}

func (c *controller) removeItem(message string, nick dggchat.User) {
	intString := strings.TrimSpace(strings.Replace(message, "-remove", "", -1))
	index, err := strconv.Atoi(intString)
	index--
	if err != nil {
		c.sendMsg("please enter a valid integer", nick.Nick)
		return
	}

	entry, err := c.dj.EntryAtIndex(index)
	if err != nil {
		c.sendMsg("Index out of range", nick.Nick)
		return
	}

	if nick.Nick != entry.Owner && !c.isMod(nick.Nick) && !nick.HasFeature("moderator") {
		c.sendMsg(fmt.Sprintf("I can't allow you to do that, %v", nick), nick.Nick)
		return
	}

	err = c.dj.RemoveIndex(index)
	if err != nil {
		c.sendMsg("index out of range", nick.Nick)
		return
	}
	saveStruct(c.dj.Queue(), *queueSaveLocation)
	c.playlistDirty = true
	c.sendMsg("Successfully removed item at index", nick.Nick)

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
		url = hasteURL + "/raw/" + res.response.Key
	case <-time.After(1 * time.Second):
		// TODO: find a better way to do this
		err = ioutil.WriteFile(tmpFile, []byte(text), 0644)
		if err != nil {
			return url, err
		}
		file, err = os.Open(tmpFile)
		if err != nil {
			return url, err
		}
		url, err = fileupload.UploadToHost(uguuURL, file)
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
		log.Printf("[MSG] message sent to %v: %v", msg.nick, msg.message)
		time.Sleep(time.Millisecond * 450)
	}
}

func (c *controller) newSong(entry opendj.QueueEntry) {
	c.playlistDirty = true
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

func (c *controller) songOver(entry opendj.QueueEntry, err error) {
	c.playlistDirty = true
	log.Println("[INFO] 🛑 Done Playing")

	queue := c.dj.Queue()
	saveStruct(queue, *queueSaveLocation)

	if len(queue) <= 0 && len(c.backupSongs) > 0 {
		rand.Seed(time.Now().Unix())
		c.dj.AddEntry(c.backupSongs[rand.Intn(len(c.backupSongs))])
	}

	likes := len(c.likes.Users)
	if likes > 0 {
		entry.Dedication = ""
		entry.Owner = "afk bot"
		c.backupSongs = append(c.backupSongs, entry)
		saveStruct(c.backupSongs, *backupSongsLocation)
		ppl := "people"
		if likes == 1 {
			ppl = "person"
		}
		c.sendMsg(fmt.Sprintf("%v %v really liked your song PeepoHappy", likes, ppl), entry.Owner)
	}
	c.likes.clear()
}

func (c *controller) songError(err error) {
	log.Printf("[ERROR] there was an error during song playback: %v", err)
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

func (c *controller) createYTQueueEntry(id string, owner string) (entry opendj.QueueEntry, err error) {
	res, err := c.ytServ.Videos.List("id,snippet,contentDetails").Id(id).Do()
	if err != nil {
		return entry, err
	}
	if len(res.Items) <= 0 {
		return entry, errors.New("nothing found")
	}
	songDuration, _ := time.ParseDuration(strings.ToLower(res.Items[0].ContentDetails.Duration[2:]))
	var video opendj.Media
	video.Title = res.Items[0].Snippet.Title
	video.Duration = songDuration
	video.URL = ytURLStart + res.Items[0].Id

	entry.Media = video
	entry.Owner = owner
	return entry, nil
}

func (c *controller) addSongToBackup(url string, nick string) error {
	if !c.isMod(nick) {
		c.sendMsg("you can't do that PepoBan", nick)
		return nil
	}
	id, err := ytIDfromURL(url)
	if err != nil {
		return err
	}
	entry, err := c.createYTQueueEntry(id, "")
	if err != nil {
		return err
	}
	c.backupSongs = append(c.backupSongs, entry)
	err = saveStruct(c.backupSongs, *backupSongsLocation)
	if err != nil {
		return err
	}
	c.sendMsg(fmt.Sprintf("added \"%v\" to the backup playlist", entry.Media.Title), nick)
	return nil
}

func ytIDfromURL(urlstring string) (id string, err error) {
	id = regexp.MustCompile(`(\?v=|be\/)[a-zA-Z0-9-_]+`).FindString(urlstring)
	if len(id) < 3 {
		err = errors.New("no valid ID found")
	}
	id = id[3:]
	return id, err
}

func (c *controller) addToBackup(entry opendj.QueueEntry) error {
	for _, song := range c.backupSongs {
		if song.Media.URL == entry.Media.URL {
			return errors.New("entry already in backup list")
		}
	}
	c.backupSongs = append(c.backupSongs, entry)
	return nil
}
