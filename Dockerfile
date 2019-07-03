FROM golang as builder
ENV GO111MODULE=on
WORKDIR /radio
ADD go.mod go.sum /radio/
RUN go mod download
ADD . .
RUN go build -o /radio .

FROM jrottenberg/ffmpeg 
WORKDIR /
RUN wget https://yt-dl.org/downloads/latest/youtube-dl -O /usr/local/bin/youtube-dl
RUN sudo chmod a+rx /usr/local/bin/youtube-dl
COPY --from=builder /radio /usr/bin/radio
ENTRYPOINT ["/usr/bin/radio"]