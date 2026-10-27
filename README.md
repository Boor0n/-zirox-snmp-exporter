# Zirox SNMP Exporter

SNMP-экспортер для Prometheus с веб-интерфейсом. Устройства добавляются в браузере. При добавлении устройство опрашивается **один раз целиком**: находятся порты, CPU, память, датчики температуры, БП и вентиляторы. Дальше по расписанию опрашиваются **только отмеченные галочками** объекты, точечными SNMP GET без walk. Prometheus забирает значения из памяти экспортера и к устройствам не обращается.

## Как это работает

1. **Добавление / Refresh.** Полный опрос: `ifTable`/`ifXTable`, CISCO-PROCESS-MIB, CISCO-MEMORY-POOL-MIB, CISCO-SYSTEM-EXT-MIB (Nexus), ENTITY-SENSOR-MIB, CISCO-ENVMON-MIB, HOST-RESOURCES-MIB, UCD-SNMP-MIB, CISCO-FIREWALL-MIB и CISCO-REMOTE-ACCESS-MONITOR-MIB (ASA), BGP4-MIB.
2. **Выбор.** В карточке устройства все объекты показаны списком с галочками. По умолчанию отмечены физические порты в состоянии up и все объекты здоровья. Порты в состоянии down и VLAN-интерфейсы не отмечены.
3. **Опрос.** Только отмеченные объекты, пачками до 30 OID в одном GET. Для 48-портового коммутатора с 10 отмеченными портами это 4 небольших запроса за интервал.
4. **Refresh.** Повторный полный опрос по кнопке или по расписанию (Settings → Scheduled refresh, например раз в неделю ночью; по умолчанию выключено). Новые порты подсвечиваются и, если они up, сразу отмечаются (это настраивается). Пропавшие помечаются и больше не опрашиваются. Ваши галочки сохраняются, порты сопоставляются по имени (`ifName`), а не по `ifIndex`.

### Частота опроса

Интервал задаётся в секундах на трёх уровнях, более конкретный побеждает:

| Уровень | Где задаётся |
| --- | --- |
| Устройство | Карточка устройства → «Изменить» → «Свой интервал» |
| Профиль опроса | Poll profiles (по умолчанию: Fast 6 s, Normal 60 s, Slow 300 s) |
| Глобальный | «Настройки» (по умолчанию 60 с) |

Изменения применяются кнопкой сразу, без перезапуска. Минимум 5 с. Если устройство не отвечает, интервал для него временно увеличивается (2×, 4×… до 10 минут), чтобы не долбить недоступный хост. Не более 128 одновременных опросов и 32 одновременных полных опросов (`ZIROX_MAX_POLLS`, `ZIROX_MAX_DISCOVERY`). Это ограничение одновременной работы, а не числа устройств: остальные устройства ждут своей очереди доли секунды. Подробнее в разделе «Сколько устройств выдержит».

## Запуск

### Установка одной командой

На Linux-сервере с Docker (или без него, тогда ставится сервис systemd). Репозиторий приватный, поэтому нужен токен GitHub с правом чтения: GitHub → Settings → Developer settings → Fine-grained tokens → Generate new token, Repository access: только этот репозиторий, Permissions → Contents: Read-only.

```bash
T=github_pat_...   # ваш токен
curl -fsSL -H "Authorization: Bearer $T" -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/Boor0n/-zirox-snmp-exporter/contents/deploy/get.sh | sudo GITHUB_TOKEN=$T sh
```

Скрипт берётся через API GitHub: `raw.githubusercontent.com` для приватного репозитория с fine-grained токеном отвечает 404. Если интернет доступен только через прокси, передайте его и `curl`, и скрипту: `curl -x http://прокси:порт ... | sudo GITHUB_TOKEN=$T https_proxy=http://прокси:порт sh`. Docker при установке ничего не скачивает: образ приходит файлом.

Скрипт скачивает последний релиз, проверяет SHA256, загружает образ в Docker и запускает контейнер `zirox` (`--restart unless-stopped`), ждёт ответа веб-интерфейса и выводит адрес и пароль admin. Данные лежат в `/var/lib/zirox`, настройки в `/etc/zirox/zirox.env`.

- Обновление: та же команда ещё раз. База, ключ и настройки сохраняются.
- Опции добавляются после `sh -s --`, например `... | sudo GITHUB_TOKEN=$T sh -s -- --port 9200`:
  - `--systemd` поставить бинарник как сервис systemd вместо Docker (по умолчанию, если Docker не запущен);
  - `--version v0.3.0` конкретная версия вместо последней;
  - `--port N` порт веб-интерфейса и `/probe` (по умолчанию 9116);
  - `--uninstall` удалить контейнер или сервис, данные остаются.

### Docker из исходников

```bash
docker compose up -d --build
docker logs zirox   # если ZIROX_ADMIN_PASSWORD пуст, здесь будет сгенерированный пароль admin
```

Откройте `http://<сервер>:9116`.

### Без Docker (systemd)

Скачайте архив `zirox-<версия>-linux-amd64.tar.gz` со страницы Releases репозитория (для ARM-серверов `linux-arm64`), затем:

```bash
tar -xzf zirox-*-linux-amd64.tar.gz && cd zirox-*-linux-amd64
sudo ./install.sh
```

Скрипт ставит бинарник в `/usr/local/bin/zirox`, создаёт пользователя `zirox`, сервис systemd, файл настроек `/etc/zirox/zirox.env` со случайным паролем admin (пароль выводится в конце) и запускает сервис. Данные лежат в `/var/lib/zirox`.

- Обновление: распакуйте новый архив и снова запустите `sudo ./install.sh`. База, ключ и настройки сохраняются.
- Удаление: `sudo ./install.sh --uninstall` (данные остаются).
- Из исходников: `sudo deploy/install.sh` в клоне репозитория соберёт бинарник сам (нужен Go 1.26+).

Новый релиз собирается автоматически при push тега (`git tag v0.2.0 && git push origin v0.2.0`) или кнопкой на GitHub: Actions → release → Run workflow, указать версию.

### Переменные окружения

| Переменная | По умолчанию | Назначение |
| --- | --- | --- |
| `ZIROX_LISTEN` | `:9116` | Адрес HTTP-сервера |
| `ZIROX_DATA_DIR` | `./data` | База SQLite и ключ шифрования |
| `ZIROX_ADMIN_USER` | `admin` | Логин администратора (создаётся при первом запуске) |
| `ZIROX_ADMIN_PASSWORD` | случайный | Пароль администратора при первом запуске |
| `ZIROX_RESET_ADMIN_PASSWORD` | — | `1` = сбросить пароль на `ZIROX_ADMIN_PASSWORD` при старте |
| `ZIROX_SECRET_KEY` | файл `data/secret.key` | 32 байта в base64 для шифрования community и паролей v3 |
| `ZIROX_SCRAPE_TOKEN` | — | Требовать `Authorization: Bearer <token>` на `/probe`, `/sd`, `/metrics` |
| `ZIROX_SD_ADDRESS` | Host из запроса | `host:port`, по которому Prometheus ходит к экспортеру |
| `ZIROX_COOKIE_SECURE` | — | `1`, если UI открыт через HTTPS |
| `ZIROX_TLS_CERT`, `ZIROX_TLS_KEY` | — | Включить HTTPS прямо в экспортере |
| `ZIROX_MAX_POLLS` | `128` | Одновременных опросов |
| `ZIROX_MAX_DISCOVERY` | `32` | Одновременных полных опросов |

**Делайте резервную копию `data/secret.key`** вместе с базой: без ключа сохранённые community и пароли не расшифровать.

## Сколько устройств выдержит

Число устройств не ограничено. У каждого устройства свой цикл опроса, а `ZIROX_MAX_POLLS` и `ZIROX_MAX_DISCOVERY` ограничивают только, сколько запросов идёт одновременно. Нагрузочный тест (`ZIROX_SCALE=1000 ZIROX_SCALE_PORTS=200 go test ./internal/engine -run TestScale -v`): коммутаторы, у которых половина портов поднята и отмечена для опроса, задержка сети 20 мс, интервал 10 с. Память указана для самого Zirox, без имитаторов устройств.

| Устройств | Портов на устройство | Портов всего | Полный опрос всех | Все опрошены за интервал | Память |
|---|---|---|---|---|---|
| 300 | 48 | 14 400 | 6 с | да | 17 МБ |
| 1000 | 48 | 48 000 | 16 с | да | 60 МБ |
| 1000 | 200 | 200 000 | 17 с | да | 240 МБ |
| 1000 | 500 | 500 000 | 27 с | да | 650 МБ |

Память растёт примерно на 1,3 КБ на отмеченный порт. При задержке сети 100 мс и 1000 устройств по 48 портов полный опрос всех занимает около 1 мин 17 с, регулярный опрос успевает в интервал.

Узким местом при сотнях тысяч портов будет не Zirox, а Prometheus: каждый отмеченный порт даёт около 11 рядов. Например, 250 000 отмеченных портов дают около 2,7 млн рядов, а Prometheus расходует на один ряд несколько килобайт памяти. Отмечайте только нужные порты, задавайте интервал 60 с для доступа и чаще только для магистралей, а для долгого хранения используйте VictoriaMetrics или Mimir.

Для нескольких тысяч устройств или медленных каналов поднимите `ZIROX_MAX_POLLS` (например 256) в `/etc/zirox/zirox.env` и перезапустите той же командой установки.

## Prometheus

```yaml
scrape_configs:
  - job_name: zirox-snmp
    http_sd_configs:
      - url: http://zirox-host:9116/sd
        refresh_interval: 60s
```

Через HTTP Service Discovery Prometheus получает список устройств и интервал каждого (`__scrape_interval__`). Новое устройство из веб-интерфейса появляется в Prometheus без правки конфига. Каждое устройство — отдельная цель с метками `instance`, `device`, `group`, `address`, `sysName`.

| Эндпоинт | Что отдаёт |
| --- | --- |
| `/sd` | Список целей для HTTP SD (JSON) |
| `/probe?target=<id\|имя\|IP>` | Метрики одного устройства из кэша |
| `/metrics/all` | Все устройства одним ответом, с метками `device`, `group` |
| `/metrics` | Состояние самого экспортера |
| `/healthz` | Проверка живости |

### Метрики

Порты (имена как у стандартного snmp_exporter, готовые дашборды Grafana подходят): `ifHCInOctets`, `ifHCOutOctets`, `ifHCInUcastPkts`, `ifHCOutUcastPkts`, `ifInErrors`, `ifOutErrors`, `ifInDiscards`, `ifOutDiscards`, `ifOperStatus`, `ifAdminStatus`, `ifHighSpeed` с метками `ifIndex`, `ifName`, `ifDescr`, `ifAlias`. Для SNMPv1 вместо 64-битных счётчиков отдаются `ifInOctets`/`ifOutOctets`/`ifSpeed`.

Здоровье устройства: `zirox_cpu_utilization_percent{window}`, `zirox_memory_used_bytes`, `zirox_memory_free_bytes`, `zirox_memory_utilization_percent`, `zirox_memory_total_bytes`, `zirox_memory_available_bytes`, `zirox_temperature_celsius`, `zirox_sensor_value{unit}`, `zirox_sensor_status`, `zirox_envmon_{temperature,fan,psu}_state`, `zirox_storage_{size,used}_bytes`.

Фаерволы (ASA): `zirox_firewall_connections{type="current"|"max"}`, `zirox_vpn_sessions{type="all"|"l2l"|"anyconnect"}`, `zirox_vpn_users`.

BGP-соседи (BGP4-MIB, IPv4): `zirox_bgp_peer_state` (6 = established), `zirox_bgp_peer_admin_status`, `zirox_bgp_peer_established_seconds`, `zirox_bgp_peer_updates_received_total` с метками `peer`, `remote_as`.

Служебные: `zirox_poll_success`, `zirox_poll_duration_seconds`, `zirox_poll_last_success_timestamp_seconds`, `zirox_poll_interval_seconds`, `zirox_poll_samples`, `zirox_device_info`.

Примеры запросов:

```promql
rate(ifHCInOctets{device="core-nx-01"}[5m]) * 8   # входящий трафик, бит/с
increase(ifInErrors[1h]) > 0                        # порты с ошибками за час
zirox_poll_success == 0                             # устройства, которые не отвечают
```

### Алерты

`deploy/alerts.yml` — готовые правила для Prometheus: устройство не отвечает 5 минут, CPU > 90 %, память > 90 %, температура > 70 °C, проблема с БП/вентилятором/датчиком (CISCO-ENVMON), диск заполнен > 90 %, порт упал при admin up, ошибки на порту, загрузка порта > 90 %, BGP-сосед не в состоянии established. Пороги правьте под себя.

```yaml
rule_files:
  - /etc/prometheus/zirox-alerts.yml
```

Проверка: `promtool check rules deploy/alerts.yml && promtool test rules deploy/alerts_test.yml`.

## Grafana

`deploy/grafana-dashboard.json` — дашборд «Zirox SNMP». Импорт: Grafana → Dashboards → New → Import → загрузить файл, выбрать источник Prometheus. Фильтры сверху: группа, устройство, интерфейс. Разделы:

- **Overview** — сколько устройств, сколько не отвечает, упавшие порты, аппаратные проблемы, таблица всех устройств со временем последнего успешного опроса;
- **Health** — CPU, память, температура, диски, состояние БП и вентиляторов;
- **Interfaces** — входящий/исходящий трафик, загрузка в %, ошибки и discards, таблица портов со статусом и скоростью;
- **Firewall and routing** — соединения и VPN-сессии ASA, таблица BGP-соседей.

Так как имена метрик портов совпадают со стандартным snmp_exporter, подходят и готовые дашборды из каталога Grafana.

## Импорт из CSV

«Импорт» принимает CSV с заголовком. Обязательна только колонка `address`. Остальные необязательные: `name`, `port`, `group`, `profile` (имя профиля), `credential` (имя сохранённых учётных данных) или `version` + `community` для v1/v2c.

```csv
name,address,group,profile,credential
core-nx-01,10.0.0.1,DC1,Fast (6 s),Nexus RO v3
acc-sw-12,10.0.1.12,DC1,,
```

## Свои OID

Если нужного OID нет среди встроенных (порты, CPU, память, датчики, диски, межсетевой экран, VPN, BGP), его можно добавить на странице **Custom OIDs**:

1. В блоке «Check an OID on a device» выберите устройство и OID. Включите **Walk**, чтобы увидеть всю таблицу или ветку. Программа сразу покажет, что отвечает устройство и какие значения числовые.
2. Нажмите **Add column**, чтобы опрашивать столбец таблицы (одно значение на строку: на порт, датчик, пул), или **Add value** для одиночного значения (обычно OID на `.0`).
3. Задайте имя метрики (например, `cisco_mempool_used_bytes`), тип (gauge или counter) и при необходимости множитель. Для таблицы можно указать столбец с именами строк: они станут меткой `name`.
4. Нажмите **Refresh all devices** или Refresh на нужном устройстве. Найденные объекты появятся в разделе **Custom** карточки устройства с галочками, как порты.

Метрика таблицы получает метки `index` и `name`, у одиночного значения меток нет. Экспортируются только числовые значения. При удалении OID его объекты пропадают со всех устройств. Все изменения записываются в журнал.

## Журнал изменений

Страница «Change log» показывает, кто и когда добавил, изменил или удалил устройство, учётные данные, профиль, поменял галочки (какие объекты добавлены и сняты), настройки и пароль, а также входы в систему (и неудачные) и запуски Refresh, в том числе по расписанию. Записи хранятся год.

## Пользователи

Страница «Users» (только для admin): пользователи с ролью **admin** (могут менять всё) или **read-only** (видят устройства, порты, настройки и журнал, но ничего не меняют, кроме своего пароля). Нельзя удалить или понизить последнего администратора. Учётная запись из `ZIROX_ADMIN_USER` создаётся при первом запуске; при обновлении со старой версии существующий admin переносится автоматически.

## REST API

Токены создаются на странице «Users» → «API tokens» и показываются один раз (в базе хранится только SHA-256). Токен read-only может только читать.

```bash
T=zx_...   # токен
H="Authorization: Bearer $T"
curl -H "$H" http://zirox:9116/api/v1/devices                 # все устройства со статусом опроса (?group=DC1)
curl -H "$H" http://zirox:9116/api/v1/devices/5               # одно устройство со всеми найденными объектами
curl -H "$H" -X POST http://zirox:9116/api/v1/devices \
     -d '{"name":"acc-sw-12","address":"10.0.1.12","group":"DC1","profile":"Fast (6 s)","credential":"Nexus RO v3"}'
curl -H "$H" -X PATCH http://zirox:9116/api/v1/devices/5 -d '{"group":"DC2","interval_sec":30,"enabled":false}'
curl -H "$H" -X POST http://zirox:9116/api/v1/devices/5/refresh
curl -H "$H" -X POST http://zirox:9116/api/v1/devices/5/selection \
     -d '{"select":["Ethernet1/5","Supervisor CPU"],"unselect":["Ethernet1/1"]}'
curl -H "$H" -X DELETE http://zirox:9116/api/v1/devices/5
curl -H "$H" http://zirox:9116/api/v1/profiles
curl -H "$H" http://zirox:9116/api/v1/credentials           # только имена и версии, без секретов
```

При создании вместо `credential` можно передать `"version":"2c","community":"..."`. Новое устройство сразу проходит полный опрос. В `selection` объекты называются ключом (имя порта или датчика, как в поле `key`) или `id`; не названные объекты не меняются. Все изменения через API попадают в журнал как `api:<имя токена>`.

## Безопасность

- Вход в UI по логину и паролю (bcrypt), сессия в подписанной cookie (`HttpOnly`, `SameSite=Strict`), POST-запросы с чужого сайта отклоняются.
- Community и пароли SNMPv3 хранятся в базе в зашифрованном виде (AES-256-GCM) и никогда не показываются в интерфейсе.
- Поддерживается SNMPv3 authPriv (SHA/SHA-2, AES/AES-192/AES-256, DES).

## Разработка

```bash
go test ./...
# Интеграционный тест с настоящим snmpd (v1, v2c, v3), см. .github/workflows/ci.yml:
ZIROX_TEST_SNMP=127.0.0.1:1161 go test ./internal/snmpx/
```

Структура: `cmd/zirox` — точка входа; `internal/snmpx` — SNMP, discovery, опрос, описание OID; `internal/engine` — расписание и кэш; `internal/exporter` — `/probe`, `/sd`, `/metrics`; `internal/web` — веб-интерфейс; `internal/store` — SQLite; `internal/secret` — шифрование.
