# ---- Build stage ----
FROM node:24-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install
COPY . .
RUN npm run build

# ---- Serve stage (nginx hosts dist and reverse-proxies /api and /v1) ----
FROM nginx:1.27-alpine AS serve
# The config is a template so the backend address can differ per topology:
# "backend:3000" under docker-compose, "127.0.0.1:3000" under ECS Fargate (where
# both containers share one network namespace and no service DNS exists).
# nginx's entrypoint runs envsubst over /etc/nginx/templates/*.template.
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
ENV BACKEND_ORIGIN=backend:3000
# Restrict substitution to our own variable. Without this filter envsubst would
# also eat nginx's runtime variables ($uri, $host, $remote_addr, $scheme,
# $proxy_add_x_forwarded_for), blanking them and breaking SPA fallback and
# proxy headers.
ENV NGINX_ENVSUBST_FILTER=^BACKEND_ORIGIN$
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
