# --- build stage ---
FROM golang:1.26 AS build
WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go build (modernc.org/sqlite needs no CGo), fully static so it runs on a
# distroless base. Templates are embedded via //go:embed, so no assets to copy.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/newsagg .

# --- runtime stage ---
# distroless/static ships CA certificates (needed for the HTTPS calls to Google
# News, article hosts, and the Anthropic API) and nothing else.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/newsagg /newsagg

# All mutable data (the SQLite DB) lives on a mounted volume at /data.
ENV DATA_DIR=/data \
    PORT=8080
EXPOSE 8080

ENTRYPOINT ["/newsagg"]
CMD ["serve"]
