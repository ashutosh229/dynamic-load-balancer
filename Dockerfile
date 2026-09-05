# --- build stage ---
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/lb .

# --- runtime stage: distroless-style minimal image ---
FROM alpine:3.20
RUN adduser -D -H lbuser
COPY --from=build /out/lb /usr/local/bin/lb
COPY config.json /etc/lb/config.json
USER lbuser
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/lb", "-config", "/etc/lb/config.json"]
