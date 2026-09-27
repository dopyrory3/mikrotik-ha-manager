FROM evilfreelancer/docker-routeros:latest

# Two things the stock image needs for a two-router lab, both documented in
# docker-compose.yml: patch its entrypoint, then wrap it so the bridged
# interface is chosen by MAC and the guest is bootstrapped for mtha.
COPY patch-image.sh /tmp/patch-image.sh
RUN sh /tmp/patch-image.sh && rm /tmp/patch-image.sh

COPY bootstrap-guest.py /usr/local/bin/bootstrap-guest.py
COPY entrypoint.sh /usr/local/bin/lab-entrypoint.sh
RUN chmod +x /usr/local/bin/bootstrap-guest.py /usr/local/bin/lab-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/lab-entrypoint.sh"]
