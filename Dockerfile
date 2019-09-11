FROM golang as builder
ENV GO111MODULE=on
WORKDIR /code
ADD go.mod go.sum /code/
RUN go mod download
ADD . .
RUN go build -o /radio .

FROM ubuntu:latest 
WORKDIR /
RUN apt update && apt install -y ffmpeg wget python
RUN wget https://yt-dl.org/downloads/latest/youtube-dl -O /usr/local/bin/youtube-dl
RUN chmod a+rx /usr/local/bin/youtube-dl
COPY --from=builder /radio /usr/bin/radio
ENTRYPOINT ["/usr/bin/radio", "-config", "/storage/config.json", "-songs", "/storage/songs.json", "-queue", "/storage/queue.json", "-users", "/storage/updateUsers.json"]
