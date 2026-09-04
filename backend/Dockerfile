# --- build stage ---
FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.buildVersion=docker" -o /out/ledgerflow ./cmd/server

# --- run stage ---
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/ledgerflow ./ledgerflow

EXPOSE 8080
ENTRYPOINT ["/app/ledgerflow"]
