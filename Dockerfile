FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev git sqlite-dev

WORKDIR /app
COPY . .

RUN rm -f go.mod go.sum || true
RUN go mod init otp-bot
RUN go get go.mau.fi/whatsmeow@latest
RUN go get go.mongodb.org/mongo-driver/mongo@latest
RUN go get github.com/lib/pq@latest
RUN go get github.com/mattn/go-sqlite3@latest
RUN go mod tidy

RUN go build -o bot .

FROM alpine:latest
RUN apk add --no-cache ca-certificates sqlite-libs

WORKDIR /app
COPY --from=builder /app/bot .

# Railway کے لیے port expose کرنا ضروری ہے
EXPOSE 8080

# Railway automatically PORT environment variable set کرتا ہے
CMD ["./bot"]