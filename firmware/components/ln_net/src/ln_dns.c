/*
 * ln_dns.c — captive-portal DNS hijack.
 *
 * Minimal UDP/53 responder on the SoftAP interface: every A query is
 * answered with 192.168.4.1 so phones resolve any hostname to the portal
 * and fire their captive-portal detection flow. Non-A queries get a
 * "no error, no answers" response so clients fall through quickly.
 */
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "esp_log.h"
#include "lwip/sockets.h"

#include "ln_net_priv.h"

static const char *TAG = "ln_dns";

#define DNS_PORT 53
#define DNS_MAX_PKT 512

static TaskHandle_t s_task;
static int s_sock = -1;
static volatile bool s_running;

/* Parse past the QNAME in a DNS question; returns offset after QTYPE/QCLASS
 * or -1 on malformed input. */
static int skip_question(const uint8_t *pkt, int len, int off)
{
    while (off < len) {
        uint8_t lab = pkt[off];
        if (lab == 0) {
            off += 1;
            break;
        }
        if ((lab & 0xC0) == 0xC0) {  /* compression pointer ends the name */
            off += 2;
            break;
        }
        off += 1 + lab;
    }
    off += 4; /* QTYPE + QCLASS */
    return (off <= len) ? off : -1;
}

static void dns_task(void *arg)
{
    uint8_t pkt[DNS_MAX_PKT + 16];

    while (s_running) {
        struct sockaddr_in from;
        socklen_t fromlen = sizeof(from);
        int n = recvfrom(s_sock, pkt, DNS_MAX_PKT, 0,
                         (struct sockaddr *)&from, &fromlen);
        if (n < 12) {
            if (!s_running) {
                break;
            }
            continue;
        }

        uint16_t qdcount = (pkt[4] << 8) | pkt[5];
        if ((pkt[2] & 0x80) || qdcount == 0) {
            continue;   /* a response, or no question — ignore */
        }

        int qend = skip_question(pkt, n, 12);
        if (qend < 0) {
            continue;
        }
        uint16_t qtype = (pkt[qend - 4] << 8) | pkt[qend - 3];

        /* Header: response, recursion-available, copy RD; one question. */
        pkt[2] = 0x80 | (pkt[2] & 0x01);
        pkt[3] = 0x80;
        pkt[4] = 0; pkt[5] = 1;          /* QDCOUNT = 1 */
        pkt[6] = 0; pkt[7] = 0;          /* ANCOUNT (set below) */
        pkt[8] = 0; pkt[9] = 0;
        pkt[10] = 0; pkt[11] = 0;

        int out = qend;
        if (qtype == 1 /* A */ || qtype == 255 /* ANY */) {
            static const uint8_t answer[] = {
                0xC0, 0x0C,             /* name: pointer to question */
                0x00, 0x01,             /* type A */
                0x00, 0x01,             /* class IN */
                0x00, 0x00, 0x00, 0x3C, /* TTL 60s */
                0x00, 0x04,             /* RDLENGTH */
                192, 168, 4, 1,         /* RDATA */
            };
            if (out + (int)sizeof(answer) <= (int)sizeof(pkt)) {
                memcpy(&pkt[out], answer, sizeof(answer));
                out += sizeof(answer);
                pkt[7] = 1;             /* ANCOUNT = 1 */
            }
        }
        sendto(s_sock, pkt, out, 0, (struct sockaddr *)&from, fromlen);
    }

    /* Task owns the socket lifetime — close only after the loop exits so no
     * other task ever closes a socket mid-recv. */
    close(s_sock);
    s_sock = -1;
    vTaskDelete(NULL);
}

esp_err_t ln_dns_start(void)
{
    if (s_task != NULL) {
        return ESP_OK;
    }
    s_sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (s_sock < 0) {
        return ESP_FAIL;
    }
    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_port = htons(DNS_PORT),
        .sin_addr.s_addr = htonl(INADDR_ANY),
    };
    if (bind(s_sock, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
        close(s_sock);
        s_sock = -1;
        return ESP_FAIL;
    }
    /* Timeout lets the task notice s_running=false on shutdown. */
    struct timeval tv = {.tv_sec = 1};
    setsockopt(s_sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

    s_running = true;
    if (xTaskCreate(dns_task, "ln_dns", 3072, NULL, 4, &s_task) != pdPASS) {
        close(s_sock);
        s_sock = -1;
        s_running = false;
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(TAG, "captive DNS up on :%d -> %s", DNS_PORT, LN_PORTAL_IP_STR);
    return ESP_OK;
}

void ln_dns_stop(void)
{
    if (s_task == NULL) {
        return;
    }
    s_running = false;
    s_task = NULL;      /* task self-deletes (and closes the socket) after
                         * the 1s recv timeout notices s_running */
}
