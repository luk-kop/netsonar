# NetSonar

NetSonar is a static network probing agent.

## Install

```bash
sudo install -m 0755 netsonar /usr/local/bin/netsonar
```

## Configure

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin netsonar
sudo mkdir -p /etc/netsonar
sudo cp config.example.yaml /etc/netsonar/config.yaml
sudo chown -R netsonar:netsonar /etc/netsonar
```

Edit `/etc/netsonar/config.yaml` before starting the service.

## Run

```bash
netsonar -config /etc/netsonar/config.yaml
```

## systemd

```bash
sudo cp netsonar.service /etc/systemd/system/netsonar.service
sudo systemctl daemon-reload
sudo systemctl enable --now netsonar
```

## Documentation

Full documentation: https://github.com/luk-kop/netsonar
