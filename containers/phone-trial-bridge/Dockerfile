FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/phone-trial-bridge ./cmd/phone-trial-bridge
COPY internal/websearch ./internal/websearch
COPY internal/conversation ./internal/conversation
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /bridge ./cmd/phone-trial-bridge
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bridge /bridge
EXPOSE 8080
ENTRYPOINT ["/bridge"]
