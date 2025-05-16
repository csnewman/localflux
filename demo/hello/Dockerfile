FROM golang:1.23

COPY go.mod ./go.mod
RUN go mod download
COPY *.go ./

RUN CGO_ENABLED=0 go build -v -o /app

CMD ["/app"]
