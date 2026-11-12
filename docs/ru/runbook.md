# Zirox: что делать, если…

Короткие инструкции для дежурного. Команды даны для установки через `get.sh` в Docker. Для systemd вместо `docker logs zirox` используйте `journalctl -u zirox`, а вместо `docker restart zirox` используйте `systemctl restart zirox`.

## Алерт ZiroxDeviceUnreachable: устройство не отвечает

1. Откройте устройство в Zirox → **Diagnostics** → **Check access**. Проверка покажет, на каком шаге обрыв: DNS, маршрут, SNMP, community, видимость таблиц.
2. С сервера Zirox выполните `snmpget -v2c -c <community> <ip> 1.3.6.1.2.1.1.5.0`.
   - Если таймаут, проблема вне Zirox: устройство недоступно, UDP 161 закрыт или адрес Zirox не входит в SNMP ACL.
   - Если ответ есть, а в Zirox таймаут, а Zirox в Docker, переустановите с `--host-network`.
3. Устройство, которое не отвечает, Zirox опрашивает реже (до раза в 10 минут). После восстановления обычный интервал вернётся сам.

## Алерт ZiroxUplinkDown: упал uplink

1. Порт с ролью `uplink` в состоянии down при административном up. Это реальная авария канала, Zirox тут ни при чём.
2. Под именем порта на странице устройства указан сосед CDP/LLDP: проверьте и его сторону.
3. Если порт больше не uplink (канал выведен), смените роль в колонке **Role** или выключите порт на устройстве (admin down).

## Алерт ZiroxInterfaceFlapping: порт флапает

Порт сменил состояние больше 3 раз за 10 минут. Проверьте оптику, кабель и ошибки на дашборде **Zirox port**: Errors and discards, Link state.

## Алерт ZiroxDellHardware или ZiroxHPHardware

Компонент сервера (диск, блок питания, вентилятор, температура) не в норме. Метка `component` говорит, что именно, имя компонента есть в метке `name`. Подробности смотрите в iDRAC или iLO самого сервера.

## Алерт ZiroxPollTooSlow: опрос не успевает

Опрос устройства занимает больше 80% интервала.
- Снимите галочки с ненужных портов (например, `^Vlan` через **Port rules**) или увеличьте интервал устройства.
- Если медленных устройств много, увеличьте `ZIROX_MAX_POLLS` в `/etc/zirox/zirox.env` и повторите команду установки.

## Алерт ZiroxPortIndexShifted: порты перенумеровались

После перезагрузки или замены модуля ifIndex указывает на другие порты. Zirox не отдаёт значения таких портов и сам запускает Refresh. Если алерт держится, нажмите **Refresh** на устройстве вручную и проверьте галочки (новые порты отмечены `new`).

## Устройство перезагрузилось

Zirox видит, что `sysUpTime` уменьшился, записывает это в **Change log** и сам делает Refresh. Галочки и роли сохраняются. На графиках в Grafana перезагрузка отмечена красной линией.

## Веб-интерфейс не открывается

```bash
curl -s http://127.0.0.1:9116/healthz   # на самом сервере; ответ ok значит, что Zirox жив
sudo docker ps -a | grep zirox          # контейнер запущен?
sudo docker logs --tail 100 zirox
```

- `healthz` отвечает, а с ПК страница не открывается: закрыт порт в firewall. Откройте его: `firewall-cmd --add-port=9116/tcp --permanent && firewall-cmd --reload`.
- Контейнер остановлен: `sudo docker start zirox`, затем посмотрите логи.

## "Too many failed logins"

После 5 неверных паролей вход закрыт на 15 минут, и для этого адреса, и для этого имени. Подождите или войдите с другого адреса под другим администратором. Блокировка видна в **Change log** как "login blocked". Если блокировки повторяются, а сами вы пароль не вводили, кто-то подбирает пароль: проверьте адрес в журнале.

## Забыт пароль admin

```bash
sudo vi /etc/zirox/zirox.env
#   ZIROX_ADMIN_PASSWORD=<новый пароль>
#   ZIROX_RESET_ADMIN_PASSWORD=1
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh
```

Войдите с новым паролем. Затем удалите строку `ZIROX_RESET_ADMIN_PASSWORD=1` и запустите установщик ещё раз, иначе пароль будет сбрасываться при каждом перезапуске.

## Скрипт получает 401 "API token expired"

Срок токена истёк. В **Users → API tokens** создайте новый и отзовите старый.

## Обновление и откат

- Обновление: команда установки ещё раз. Перед запуском новой версии база копируется в `/var/lib/zirox/backups/`.
- Откат на прошлую версию: та же команда с `sh -s -- --version vX.Y.Z`. Если новая версия уже изменила схему базы, старая её не откроет. Тогда восстановите копию из `backups/` по инструкции ниже.

## Восстановление базы

Из архива **Download backup** (или из файла в `/var/lib/zirox/backups/`):

```bash
sudo docker stop zirox
sudo rm -f /var/lib/zirox/zirox.db-wal /var/lib/zirox/zirox.db-shm
sudo tar -xzf zirox-backup-*.tar.gz -C /var/lib/zirox zirox.db secret.key
#   или: sudo cp /var/lib/zirox/backups/zirox-<дата>-<причина>.db /var/lib/zirox/zirox.db
sudo chown 10001:10001 /var/lib/zirox/zirox.db /var/lib/zirox/secret.key
sudo docker start zirox
```

`secret.key` должен быть от той же установки, иначе сохранённые community не расшифруются. Тогда их придётся ввести заново в **Credentials**.

## Кончается место на диске

- Zirox сам занимает мало: база и 5 копий в `backups/`.
- Место съедает Prometheus или VictoriaMetrics. Уменьшите `--storage.tsdb.retention.time` у Prometheus и держите долгую историю в VictoriaMetrics.
- Число серий растёт с числом отмеченных портов, примерно 11 серий на порт. Снимите галочки с ненужных портов.

## Prometheus: цель DOWN

- Prometheus не достаёт до Zirox по порту. Если он в контейнере, адрес должен быть IP сервера, а не `localhost`.
- Или задан `ZIROX_SCRAPE_TOKEN`, а Prometheus не передаёт заголовок `Authorization`.
- **Status → Targets** в Prometheus показывает точную ошибку.
