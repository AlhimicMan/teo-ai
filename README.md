# teo-ai

MVP корпоративного reverse proxy и утилиты запуска Codex CLI / Claude Code
с **личными подписками разработчиков**. Go 1.26+, только стандартная библиотека.
Клиент поддерживает macOS и Linux, сервер — Linux / Docker Compose.

## Как работает

```text
teo-ai codex  → Codex CLI   → HTTPS gateway /codex/*
                                  → https://chatgpt.com/backend-api/codex/*
teo-ai claude → Claude Code → HTTPS gateway /anthropic/*
                                  → https://api.anthropic.com/*
```

- Вход в ChatGPT / claude.ai и обновление OAuth tokens выполняет оригинальный CLI.
- Access token идёт через gateway в `Authorization` и передаётся upstream без замены.
- `ChatGPT-Account-ID`, `anthropic-beta`, `anthropic-version`, тела запросов,
  SSE, статусы ошибок и заголовки лимитов сохраняются. Gateway key удаляется.
- Gateway не читает credential stores клиентов, не хранит provider keys,
  refresh tokens, диалоги или исходный код. Учёт и лимиты подписки остаются у провайдера.
- HTTPS завершается на gateway: сервер технически видит access tokens и запросы.
  Встроенные логи содержат только маршрут, метод, HTTP status и длительность.
- OAuth/login, MCP, plugins, телеметрия и прочие соединения CLI могут обращаться
  к провайдерам напрямую. Это прокси запросов выбранного model provider,
  а не туннель всего трафика компьютера.

К серверу добавлен отдельный **gateway key** в `X-Teo-Gateway-Key`.
Это общий ключ доступа MVP, не токен ChatGPT/Claude. Он не заменяет личный вход.
У сервера только два фиксированных upstream: передать произвольный target нельзя.

## Развёртывание на сервере

Нужны Docker Engine с Compose plugin, DNS-имя сервера и доступ к upstream.
Для автоматического публичного TLS-сертификата Caddy DNS должен указывать на сервер,
а входящие TCP 80/443 должны быть доступны для ACME. Для внутреннего домена
нужен собственный TLS reverse proxy / корпоративный сертификат вместо ACME.

1. Скопировать каталог проекта на сервер и перейти в него.
2. Подготовить `.env` и gateway key:

   ```bash
   cp .env.example .env
   # В .env указать TEO_DOMAIN, ACME_EMAIL и тег TEO_GATEWAY_IMAGE.
   make key
   ```

   `make key` создаёт случайный 256-bit ключ в `secrets/gateway-key` и отказывается
   перезаписывать существующий. В нём нет provider credentials.
   Каталог `secrets` имеет права `0700`; файл `0444`, чтобы non-root процесс
   контейнера мог читать Docker Compose file secret. Защита на хосте обеспечивается
   приватным родительским каталогом. Для клиентской копии использовать `0600`.
   Если `make` отсутствует, те же действия:

   ```bash
   mkdir -p secrets
   chmod 0700 secrets
   # Только при первом создании: команда не должна перезаписывать действующий ключ.
   (umask 077; set -C; openssl rand -hex 32 > secrets/gateway-key)
   chmod 0444 secrets/gateway-key
   ```

3. Проверить и запустить:

   ```bash
   docker compose config --quiet
   docker compose pull
   docker compose up -d
   docker compose ps
   docker compose logs --tail=50 gateway
   curl --fail https://ai.example.com/healthz
   ```

   Порт Go gateway не опубликован на хосте: доступен только Caddy в Compose network.
   Контейнер gateway работает от UID 10001, с read-only filesystem и без capabilities.
   Caddy хранит сертификаты в named volumes. Для обновления повторить
   `docker compose pull` и `docker compose up -d`; `down -v` удалит сертификаты.
   Образ gateway загружается из GHCR. В `.env` можно закрепить релиз, например
   `TEO_GATEWAY_IMAGE=ghcr.io/alhimicman/teo-ai-gateway:v0.1.0`, после его выпуска.
   Тег `main` соответствует последней успешной сборке основной ветки.
   Если пакет GHCR приватный, на сервере предварительно выполнить
   `docker login ghcr.io` с GitHub credential, которому разрешено `read:packages`.

   Для сборки из исходников вместо загрузки готового образа:

   ```bash
   TEO_GATEWAY_IMAGE=teo-ai-gateway:local docker compose \
     -f compose.yaml -f compose.build.yaml up --build -d
   ```

4. Передать разработчикам gateway key отдельным защищённым каналом.
   Ни `secrets/`, ни `.env` не включаются в Docker build context и Git.

### IP, другой порт и самоподписной TLS для MVP

Для сервера без домена есть отдельный `compose.selfsigned.yaml`. Он публикует
только HTTPS-порт `TEO_TLS_PORT` (по умолчанию `8443`) и не занимает `80/443`.
Нужен Docker Compose plugin. Пример подготовки после создания gateway key:

```bash
mkdir -p tls
chmod 0700 tls
TEO_SERVER_IP=192.0.2.10
openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 365 \
  -subj '/CN=teo-ai MVP gateway' \
  -addext "subjectAltName=IP:${TEO_SERVER_IP}" \
  -keyout tls/server.key -out tls/server.crt
chmod 0600 tls/server.key
chmod 0644 tls/server.crt
# В .env задать TEO_TLS_PORT=8443 и закреплённый TEO_GATEWAY_IMAGE.
docker compose -f compose.selfsigned.yaml up -d --wait
```

Каталог `tls/` исключён из Git и Docker build context. Self-signed конфигурация
использует `deploy/Caddyfile.selfsigned`; ACME и DNS для неё не нужны.

Порт клиента задаётся в URL. В новой сборке клиента можно явно сохранить
отключение проверки TLS только для gateway:

```bash
teo-ai init --gateway https://192.0.2.10:8443 \
  --key-file /path/to/gateway-key --insecure-skip-tls-verify
teo-ai doctor
teo-ai codex
teo-ai claude
```

Настройка сохраняется как `"insecure_skip_tls_verify": true`. Разовое переопределение:
`teo-ai --insecure-skip-tls-verify doctor`; переменная окружения:
`TEO_AI_INSECURE_SKIP_TLS_VERIFY=true`. Значение `false` в явном флаге отключает
сохранённый режим. По умолчанию сертификаты проверяются. Этот флаг добавлен после
`v0.1.0`, поэтому для него нужна обновлённая сборка клиента, а сервер может
продолжать работать на gateway image `v0.1.0`.

В этом режиме обёртка на время сессии создаёт relay на `127.0.0.1` со случайным
портом. До удалённого gateway relay обращается по HTTPS с отключённой проверкой
сертификата, а native CLI использует локальный HTTP endpoint. Relay требует
gateway key, сохраняет streaming и закрывается вместе с дочерним процессом.
В `/status` CLI будет виден loopback URL; удалённый адрес остаётся в конфигурации
`teo-ai`. Штатный вход и другие HTTPS-соединения клиентов продолжают проверять TLS.
Глобальная проверка Node/OpenSSL не отключается. Этот режим предназначен для
MVP: удостоверение подлинности сертификата gateway в нём отсутствует.

### Ротация gateway key

Заменить файл новым файлом со случайным ключом, сохранить права `0444` и выполнить:

```bash
docker compose up -d --force-recreate gateway
```

Разработчики обновляют свою копию и перезапускают CLI. Ключ читается при запуске
процесса, автоматического обновления ключа в активных сессиях в MVP нет.

## Установка клиента

Установить штатные Codex CLI и Claude Code и проверить, что `codex` / `claude`
доступны в `PATH`. Этот MVP проверяет интерфейс запуска с Codex CLI 0.164.0 и
Claude Code 2.1.220; совместимость более старых и будущих версий требует проверки.
В частности, Codex должен поддерживать `--no-daemon` и custom model providers.

Готовые архивы находятся в [GitHub Releases](https://github.com/AlhimicMan/teo-ai/releases).
Доступны `linux` / `darwin` и `amd64` / `arm64`. Для Apple Silicon нужен
`darwin_arm64`, для Intel Mac — `darwin_amd64`. Скачать нужный архив и
`checksums.txt`, проверить его SHA256, распаковать и установить `teo-ai` в `PATH`.
macOS-бинарники не имеют Developer ID / notarization.

Сборка из исходников для текущего компьютера:

```bash
make build
install -m 0755 bin/teo-ai "$HOME/.local/bin/teo-ai"
```

Предварительно создать `$HOME/.local/bin`, если его нет, и добавить в `PATH`.
`make dist` собирает Linux amd64/arm64 и macOS amd64/arm64 в `dist/`.
Исполняемый файл `teo-ai-gateway` на компьютерах разработчиков не нужен.

Сохранить выданный gateway key в локальном файле с правами `0600`, затем настроить:

```bash
teo-ai init \
  --gateway https://ai.example.com \
  --key-file "$HOME/.config/teo-ai/gateway-key"

teo-ai doctor
teo-ai login codex
teo-ai login claude
```

`login` вызывает оригинальный CLI. Для Codex доступно `teo-ai login codex --device-auth`.
Нужно выбрать аккаунт с личной подпиской; API / Console login не является этим сценарием.
Обёртка не открывает и не копирует native credential stores.

`init` сохраняет только URL и абсолютный путь к gateway key в config JSON.
По умолчанию это `os.UserConfigDir()/teo-ai/config.json`:

- Linux: `$XDG_CONFIG_HOME/teo-ai/config.json` или `~/.config/teo-ai/config.json`;
- macOS: `~/Library/Application Support/teo-ai/config.json`.

Расположение можно изменить через `TEO_AI_CONFIG` или глобальный `--config`.
`--gateway` / `TEO_AI_GATEWAY_URL` и `--key-file` / `TEO_AI_GATEWAY_KEY_FILE`
переопределяют config. Глобальные флаги идут **перед** командой:

```bash
teo-ai --config ./my-config.json doctor
teo-ai --gateway https://ai.example.com codex
```

### Работа

```bash
teo-ai codex
teo-ai codex -- exec "Проверь изменения в репозитории"
teo-ai codex -- resume --last
teo-ai claude
teo-ai claude -- --resume
teo-ai claude -- -p "Объясни структуру проекта"
```

После имени клиента аргументы передаются ему без shell-интерпретации;
начальный `--` необязателен и удаляется. Рабочий каталог, stdin/stdout/stderr
и код завершения сохраняются; SIGINT/SIGTERM/SIGHUP/SIGQUIT передаются дочернему процессу.

### Настройки авторизации

Codex запускается с `requires_openai_auth=true`, `forced_login_method="chatgpt"`
и собственным provider `teo_ai`. Его HTTP/SSE transport направлен на `/codex`;
WebSocket transport отключён. Gateway key передаётся через `env_http_headers`,
provider access token поставляет сам Codex. `--no-daemon` исключает использование
общего фонового app-server, уже запущенного с другими параметрами.

Claude получает `ANTHROPIC_BASE_URL`, отдельный gateway header и
`forceLoginMethod="claudeai"`. На время запуска выключается `apiKeyHelper`,
очищаются API credentials и переключатели Bedrock/Vertex/Foundry/Mantle.
Оригинальные `~/.codex/config.toml` и `~/.claude/settings.json` не редактируются.
Остальные пользовательские и проектные настройки продолжают загружаться.

Чтобы file-based `env` не переопределил маршрут, Claude получает runtime settings
через приватный временный файл `0600` в каталоге `0700`. В argv находится только
путь; после штатного завершения или обработанного сигнала файл удаляется.
После SIGKILL/аварии файл может остаться в системном temp до очистки ОС.
Секрет также присутствует в окружении дочернего процесса. Не включать дампы
окружения или отладочное HTTP-логирование с credentials.

Это средство настройки запуска, а не принудительное ограничение пользователя:
аргументы оригинального CLI, managed settings или будущие изменения клиента могут
переопределить поведение. Managed policy имеет приоритет. Для обязательного
маршрута нужны администрируемые настройки и сетевой контроль. В MVP используются
локальные интерактивные/print/exec сессии; background/cloud/remote-control режимы
и расширения IDE отдельно не настраиваются и не проверены.

## Проверка без расходования подписки

```bash
make test
make check
```

Тесты используют локальные fake upstream и fake CLI. Проверяют маршруты и query,
неизменность provider auth/headers/body, передачу 429 и Retry-After, удаление
gateway key, streaming до завершения ответа, отмену, отсутствие секретов в логах,
private runtime settings, cleanup и exit code дочернего процесса.
Они не выполняют inference на личных аккаунтах.

Локальный Compose запускает только gateway на loopback без TLS:

```bash
make key  # один раз; если ключ уже есть, пропустить
docker compose -f compose.local.yaml up --build -d
teo-ai --gateway http://127.0.0.1:8080 --key-file ./secrets/gateway-key doctor
docker compose -f compose.local.yaml down
```

При занятом порте задать `TEO_LOCAL_PORT=18089` для Compose и использовать
`http://127.0.0.1:18089` в клиенте. Для внешнего сервера клиент требует HTTPS.
`doctor` проверяет доступность gateway, правильность gateway key и наличие CLI.
Он не проверяет доступ upstream, личный логин или модель.

### Проверка на личной подписке

После локальных тестов разработчик запускает каждый клиент, проверяет `/status`
и отправляет простой prompt. Нужно подтвердить:

1. Активен ChatGPT / claude.ai subscription login.
2. В логах gateway появился соответствующий маршрут и успешный status.
3. Работают tools, resume и длинный streaming-ответ.
4. Refresh токена выполняется клиентом после истечения срока, сессия продолжается.
5. Ошибки лимита исходной подписки возвращаются без замены на API billing.

## Параметры сервера и границы MVP

| Переменная | Значение по умолчанию |
|---|---|
| `TEO_LISTEN_ADDR` | `:8080` |
| `TEO_GATEWAY_KEY_FILE` | обязательно указать |
| `TEO_CODEX_UPSTREAM` | `https://chatgpt.com/backend-api/codex` |
| `TEO_CLAUDE_UPSTREAM` | `https://api.anthropic.com` |

Override upstream нужен для локальных integration tests или доверенной инфраструктуры;
он задаётся администратором сервера, не клиентом. `/healthz` доступен без ключа,
`/readyz` требует gateway key. Model routes требуют и gateway key, и provider Bearer
credential; запросы с `x-api-key` отклоняются. Gateway не валидирует OAuth token
самостоятельно и не может определить тариф по произвольной Bearer-строке —
проверку выполняет upstream. Он не объединяет лимиты и не переключает аккаунты.

В MVP нет SSO, индивидуальных gateway keys, per-user quotas, высокой доступности,
UI, хранения диалогов и конвертации протоколов. У key ротация общая; для
индивидуального offboarding следующая итерация — персональные gateway credentials
или mTLS. Healthcheck не подтверждает доступность провайдера.

## GitHub Actions и выпуск версий

Workflow [CI and release](https://github.com/AlhimicMan/teo-ai/actions/workflows/ci.yml)
запускается для pull requests, push в `main`, тегов `v*` и вручную через
`workflow_dispatch`.

| Событие | Результат |
|---|---|
| Pull request / ручной запуск | Тесты Linux/macOS, архивы клиента как Actions artifact, Docker build и smoke test |
| Push в `main` | Те же проверки и публикация gateway в GHCR с тегами `main` и `sha-<12 символов SHA>` |
| Push `vX.Y.Z` | Те же проверки, версионный Docker image и GitHub Release с архивами клиента |
| Push `vX.Y.Z-rc.1` | Prerelease с версионным образом, без обновления `latest` |

На Linux и macOS выполняются `gofmt`, `go vet` и `go test -race -count=1`.
Перед публикацией образ дополнительно запускается с read-only filesystem,
без capabilities, с отдельным тестовым gateway key. Проверяются healthcheck,
отказ без ключа и авторизованный `/readyz`. Личные подписки и provider tokens
не нужны ни одному CI job.

Публикация разрешена только push-событиям и зависит от успешных тестов,
упаковки бинарников и Docker smoke test. PR не публикуют образы или releases.
Actions закреплены по commit SHA; jobs проверки имеют только `contents: read`,
публикация образа — `packages: write`, публикация release — `contents: write`.
Используется встроенный `GITHUB_TOKEN`; дополнительные repository secrets не нужны.
GitHub Actions должны быть включены, а настройки аккаунта/организации должны
разрешать публикацию пакетов и создание релизов этим токеном.

Gateway image: `ghcr.io/alhimicman/teo-ai-gateway`, платформы
`linux/amd64` и `linux/arm64`. Тег `v0.1.0` также получает alias `0.1.0`;
стабильный выпуск обновляет `latest`. Образ построен на `scratch` с CA bundle,
статическим Go executable и UID/GID `10001:10001`; QEMU для сборки не требуется.

Для выпуска новой версии после успешного CI на `main`:

```bash
git switch main
git pull --ff-only
git tag -a v0.1.0 -m 'Release v0.1.0'
git push origin v0.1.0
```

Использовать свободную версию. Поддерживается формат `vMAJOR.MINOR.PATCH`
и prerelease suffix, например `v0.2.0-rc.1`. Workflow сначала публикует image,
затем создаёт draft release, загружает assets и публикует release. При сбое
можно повторить workflow: незавершённый draft будет заполнен, уже опубликованный
release повторно не изменяется. Tags опубликованных версий перемещать не следует.

В release прикладываются:

```text
teo-ai_v0.1.0_linux_amd64.tar.gz
teo-ai_v0.1.0_linux_arm64.tar.gz
teo-ai_v0.1.0_darwin_amd64.tar.gz
teo-ai_v0.1.0_darwin_arm64.tar.gz
checksums.txt
```

Каждый архив содержит `teo-ai` и README. `teo-ai version` показывает версию,
commit и время сборки. Бинарник gateway отдельно не выпускается — сервер
распространяется как Docker image. Архивы последней CI-сборки также доступны
в Actions artifacts и хранятся 14 дней.

Локальная проверка упаковки:

```bash
VERSION=v0.1.0 make release
cd dist/release
# Linux: sha256sum --check checksums.txt
shasum -a 256 --check checksums.txt
```

## Источники протокола

- [OpenAI: ChatGPT sign-in through a gateway](https://learn.chatgpt.com/docs/enterprise/sign-in-with-chatgpt-through-a-gateway)
- [OpenAI: authentication and local refresh](https://learn.chatgpt.com/docs/auth)
- [Claude Code: subscriptions and gateways](https://code.claude.com/docs/en/llm-gateway#subscriptions-and-gateways)
- [Claude Code: gateway compatibility](https://code.claude.com/docs/en/llm-gateway-protocol)
- [Claude Code: CLI settings](https://code.claude.com/docs/en/cli-reference)

Настройка основана на документации, проверенной 27 октября 2026 года.
End-to-end inference, лимиты и реальный OAuth refresh нужно подтвердить
на целевых аккаунтах и сервере: автоматические тесты этого не доказывают.
