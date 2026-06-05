# Build a static binary, then ship it with the client assets and level files.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/webshooter .

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /bin/webshooter /app/webshooter
COPY static/ /app/static/
COPY levels/ /app/levels/
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/webshooter"]
