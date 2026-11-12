# Zirox SNMP Exporter: руководство

Zirox опрашивает сетевые устройства и серверы по SNMP и отдаёт метрики в Prometheus. Устройства добавляются и настраиваются в веб-интерфейсе (он на английском). Полный обход устройства выполняется только при добавлении и по кнопке **Refresh**. Регулярно опрашиваются только отмеченные галочкой порты и датчики.

Английская документация с полным списком метрик и переменных: [README.md](../../README.md). Что делать при сбоях: [runbook.md](runbook.md).

## Установка

Нужен Linux-сервер (RHEL/Rocky/Alma, Ubuntu, Debian) с `curl`, `sudo` и, желательно, Docker. До устройств должен быть открыт UDP 161. До сервера из браузера и из Prometheus нужен TCP 9116 или другой выбранный порт.

```bash
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh
```

Через прокси:

```bash
P=http://proxy.example.com:8080
curl -fsSL -x "$P" https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo https_proxy=$P sh
```

Опции пишутся после `sh -s --`:

| Опция | Зачем |
| --- | --- |
| `--port 9117` | Если 9116 занят (например, старым snmp_exporter). |
| `--host-network` | Контейнер работает в сети хоста. Это нужно, если с хоста `snmpget` проходит, а Zirox получает "no route to host" или таймауты. |
| `--systemd` | Поставить бинарник как службу systemd вместо Docker. |
| `--version v0.8.0` | Поставить конкретную версию, например чтобы откатиться. |
| `--uninstall` | Удалить контейнер или службу. Данные остаются. |

**Обновление:** та же команда ещё раз. База, ключ, порт и режим сети сохраняются. Перед обновлением Zirox сам делает копию базы в `/var/lib/zirox/backups/`.

Где что лежит:
- `/var/lib/zirox/zirox.db`: база.
- `/var/lib/zirox/secret.key`: ключ шифрования community и паролей.
- `/etc/zirox/zirox.env`: настройки и пароль admin для первого входа.

## Первые шаги

1. Откройте `http://<сервер>:9116` и войдите как `admin`. Пароль установщик печатает в конце работы, его также можно посмотреть командой `sudo grep ADMIN_PASSWORD /etc/zirox/zirox.env`.
2. **Credentials → New**: имя, версия SNMP и community (v1/v2c) или пользователь и пароли (v3). Community без символа `@`: Cisco читает `@` как контекст.
3. **Devices → Add device**: адрес, имя и группа (сайт). Имя станет меткой `device` в Prometheus.
   - **Check access** проверяет доступ по шагам: DNS, маршрут, ответ по SNMP, community, видимость таблиц. На каждой ошибке показывает, что исправить, с готовой конфигурацией для IOS, NX-OS, IOS XR и ASA.
   - **Add and discover** выполняет полный обход.
4. На странице устройства отметьте нужные порты и датчики и нажмите **Apply**. Порты в состоянии up отмечены по умолчанию, down нет.
5. Много устройств сразу можно добавить через **Import CSV** или **Find devices** (сканирование подсети до /20).

## Роли портов, правила и соседи

- У порта может быть **роль**: `uplink`, `core`, `server`, `access` или своё слово. Роль экспортируется меткой `role`.
- Порты с ролью `uplink` опрашиваются чаще, по умолчанию раз в 10 с (**Settings → Uplink interval**). Для них в Prometheus появляется отдельная цель с меткой `poll="fast"`.
- **Port rules** ставят галочку и роль новым портам по шаблону:
  - по имени, например `^Vlan` → снять галочку;
  - по описанию, например `UPLINK|CORE` → uplink;
  - по имени соседа CDP/LLDP, например `^esx-` → server.
- Правила проверяются сверху вниз, срабатывает первое подходящее. **Apply to existing ports** применяет их к уже известным портам.
- **Соседи CDP/LLDP** видны под именем порта и ведут на страницу соседа, если он тоже есть в Zirox.
- Порт, за которым стоит другое устройство из Zirox, сам становится uplink. Отключается в **Settings**.
  - Когда добавляете второе из двух соединённых устройств, нажмите **Refresh** на первом: тогда и его порт станет uplink.
  - Если роль убрать вручную, она больше не вернётся.

## Prometheus

```yaml
scrape_configs:
  - job_name: zirox-snmp
    http_sd_configs:
      - url: http://<сервер-zirox>:9116/sd
        refresh_interval: 60s
```

Каждое устройство становится отдельной целью со своим интервалом. Новые устройства появляются сами. Если Prometheus работает в контейнере, а Zirox запущен с `--host-network`, укажите IP сервера, а не `localhost`.

Алерты лежат в `deploy/alerts.yml`. Их нужно подключить в `rule_files`. Основные:
- устройство не отвечает;
- **uplink упал** (critical, через минуту);
- порт флапает;
- CPU, память, температура;
- железо Cisco, Dell и HP;
- BGP;
- опрос не успевает за интервал.

Чтобы уведомления шли через Grafana, откройте **Alerting → Alert rules → More → Import to Grafana-managed rules**.

## Grafana

Четыре связанных дашборда:
- **Zirox NOC**: все устройства;
- **Zirox device**: одно устройство;
- **Zirox port**: один порт;
- **Zirox health**: сам Zirox.

Щелчок по устройству или порту открывает следующий уровень. На графиках отмечены перезагрузки устройств (красным) и Refresh (синим).

Загрузка одной командой. Нужен токен сервисного аккаунта Grafana с ролью Editor: **Administration → Users and access → Service accounts**.

```bash
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/grafana/upload.sh |
  GRAFANA_URL=http://127.0.0.1:3000 GRAFANA_TOKEN=glsa_... sh
```

## Долгая история: VictoriaMetrics

Prometheus оставляет себе сбор и алерты и копирует данные в VictoriaMetrics через `remote_write`. VictoriaMetrics хранит годы истории и занимает в разы меньше места.

```bash
docker run -d --name victoriametrics --restart unless-stopped -p 8428:8428 \
  -v /var/lib/victoria-metrics:/victoria-metrics-data \
  victoriametrics/victoria-metrics:v1.102.1 -retentionPeriod=5y
```

```yaml
# prometheus.yml
remote_write:
  - url: http://<сервер-vm>:8428/api/v1/write
```

В Grafana добавьте источник данных типа Prometheus с адресом `http://<сервер-vm>:8428`. Дашборды работают с ним без изменений.

Объём для оценки: 300 устройств по 50 портов дают около 40–50 ГБ в год.

## Резервные копии и перенос

**Backup and config** в меню:
- **Download backup**: архив с базой и ключом. Внутри файл `RESTORE.txt` с командами восстановления. Храните архив как пароли: в нём ключ к community.
- **Export without secrets / with secrets**: конфигурация в YAML (устройства, галочки, роли, правила, учётные данные). Её можно положить в git и импортировать на другом сервере через **Import a configuration**: сначала Preview, потом Import.

Автоматические копии базы лежат в `/var/lib/zirox/backups/`, хранятся последние 5. Они делаются перед каждым обновлением и импортом.

## Безопасность

- После 5 неверных паролей за 15 минут вход с этого адреса и для этого имени закрыт на 15 минут. Блокировка видна в **Change log** как "login blocked".
- Пользователи бывают двух видов: admin и read-only (**Users**).
- API-токены выдаются на 30, 90 дней, год или бессрочно. Просроченный токен получает 401.
- Community и пароли v3 хранятся в базе в зашифрованном виде и в интерфейсе не показываются.
