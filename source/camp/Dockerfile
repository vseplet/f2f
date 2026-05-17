# syntax=docker/dockerfile:1.7

# camp has zero runtime dependencies — Bun ships with everything we use
# (Bun.serve, WebSocket, fetch). Skipping the install step keeps the
# image minimal; if we ever add a real npm dep, reintroduce a deps stage.

FROM oven/bun:1-slim

WORKDIR /app

ENV NODE_ENV=production
ENV PORT=8080

# Source is small and rarely changes by layer — flat COPY is fine.
COPY src ./src
COPY package.json tsconfig.json ./

EXPOSE 8080
EXPOSE 3478/udp

# The bun image ships with a non-root `bun` user. Use it.
USER bun

# Bun runs TypeScript directly, no transpile step.
CMD ["bun", "src/server.ts"]
