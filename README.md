[![Go Report Card](https://goreportcard.com/badge/github.com/SoMuchForSubtlety/strims-radio)](https://goreportcard.com/report/github.com/SoMuchForSubtlety/strims-radio)
# strims-radio

strims-radio is a plug.dj clone that uses [opendj](https://github.com/SoMuchForSubtlety/opendj) and the youtube API to stream songs requested by chat to a RTMP server.
## setup
    git clone https://github.com/SoMuchForSubtlety/strims-radio
    cd strims-radio
    cp sample-config.json config.json
    vim config.json
    go run .
## flags
      -config string
        the location of the config file (default "config.json")
## commands
Commands only work in private messages. These commands are currently available:
    
    -playing
        replies with currently playing song
    -next
        replies with next song
    -queue
        replies with the number of songs in the queue and the users position if applicable
    -playlist
        replies with a link to the current queue
    -updateme
        turns on/off notifications for the user when a new song starts playing
    -like
        total likes are told to the song submitter once it finishes playing
    -dedicate {other user}
        dedicates the users next song in the queue to the provided user
    -remove {int}
        removes the song at the provided index from the queue if the user submitted it or the user is a mod
    {youtube url}
        adds the song at the provided url to the queue if it's not too long
