# syntax=docker/dockerfile:1.7
#
# Build context is source/ (NOT source/camp), because camp imports the
# wire types from the helper module. Deploy from the repo root with:
#
#   flyctl deploy source --config camp/fly.toml -a f2f-camp
#
# (or just `make camp-deploy`).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY helper ./helper
COPY camp ./camp
WORKDIR /src/camp
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /camp .

FROM scratch
COPY --from=build /camp /camp
ENV PORT=8080
EXPOSE 8080
EXPOSE 3478/udp
ENTRYPOINT ["/camp"]
