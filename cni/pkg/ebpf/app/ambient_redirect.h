// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#ifndef __AMBIENT_REDIRECT_H__
#define __AMBIENT_REDIRECT_H__

#define ENABLE_IPV4

enum {
    LOG_NONE = 0,
    LOG_INFO = 1,
    LOG_DEBUG = 2,
};

#define OUTBOUND_CB (4321)
#define INBOUND_CB (1234)
#define BYPASS_CB (0xC001F00D)

#define ZTUNNEL_INBOUND_MARK (5678)
#define ZTUNNEL_OUTBOUND_MARK (8765)

#define ZTUNNEL_TPROXY_MARK (1024)

#define ZTUNNEL_INBOUND_PORT (15008)
#define ZTUNNEL_INBOUND_PLAINTEXT_PORT (15006)
#define ZTUNNEL_OUTBOUND_PORT (15001)

// Limited to 1K pods per node
#define MAX_PODS_PER_NODE (1024)
#define APP_INFO_MAP_SIZE MAX_PODS_PER_NODE

#define ETH_ALEN 6
#define BPF_F_CURRENT_NETNS (-1L)
#define TC_ACT_OK       0
#define TC_ACT_SHOT     2
#define ETH_P_IP        (0x0800)
#define ETH_P_IPV6      (0x86dd)
#define UDP_P_DNS       (53)
// #define PIN_GLOBAL_NS   2

/* Number of extension headers that can be skipped */
#define IPV6_MAX_HEADERS        1

#define NEXTHDR_HOP             0       /* Hop-by-hop option header. */
#define NEXTHDR_TCP             6       /* TCP segment. */
#define NEXTHDR_UDP             17      /* UDP message. */
#define NEXTHDR_IPV6            41      /* IPv6 in IPv6 */
#define NEXTHDR_ROUTING         43      /* Routing header. */
#define NEXTHDR_FRAGMENT        44      /* Fragmentation/reassembly header. */
#define NEXTHDR_GRE             47      /* GRE header. */
#define NEXTHDR_ESP             50      /* Encapsulating security payload. */
#define NEXTHDR_AUTH            51      /* Authentication header. */
#define NEXTHDR_ICMP            58      /* ICMP for IPv6. */
#define NEXTHDR_NONE            59      /* No next header */
#define NEXTHDR_DEST            60      /* Destination options header. */
#define NEXTHDR_SCTP            132     /* SCTP message. */
#define NEXTHDR_MOBILITY        135     /* Mobility header. */

#define NEXTHDR_MAX             255


#define CAPTURE_DNS_FLAG (1<<0)

#ifndef __inline
#define __inline                         \
   inline __attribute__((always_inline))
#endif

struct ztunnel_info {
    __u32  ifindex;
    __u8   mac_addr[ETH_ALEN];
    __u8   flag;
    __u8   pad;
};
struct app_info {
    __u32  ifindex;
    __u8   mac_addr[ETH_ALEN];
    __u8   pads[2];
};

struct host_info {
    __u32 addr[4];
};

#endif // __AMBIENT_REDIRECT_H__