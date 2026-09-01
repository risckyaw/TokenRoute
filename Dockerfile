FROM golang:1.27-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /tokenroute ./cmd/tokenroute

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /tokenroute /tokenroute
EXPOSE 8400
USER nonroot
ENTRYPOINT ["/tokenroute", "serve", "--config", "/config/config.yaml"]
