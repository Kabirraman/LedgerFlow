# --- deps stage ---
FROM node:20-bookworm-slim AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

# --- build stage ---
FROM node:20-bookworm-slim AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

# --- run stage ---
FROM node:20-bookworm-slim AS run
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

# `output: 'standalone'` in next.config.mjs produces a self-contained server,
# so the image doesn't need the full node_modules tree.
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
# No public/ directory exists yet; add one if static assets are introduced later.

EXPOSE 3000
CMD ["node", "server.js"]
