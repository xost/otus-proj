FROM golang:1.21

ADD ./app /app

WORKDIR /app

RUN go mod tidy
RUN go mod vendor
RUN GOOS=linux GOARG=amd64 go build -o app

CMD ["/app/app"]
