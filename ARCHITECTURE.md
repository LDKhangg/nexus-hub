---
title: Nexus-Hub Architecture & Design Specification
project: nexus-hub
tags:
  - architecture
  - golang
  - ai-agent
  - hitl
  - system-design
status: active
created: 2026-10-24
---
---
title: Nexus-Hub Master Architecture & MVP Specification
project: nexus-hub
version: 3.0.0-mvp
tags:
  - architecture
  - golang
  - ddd
  - bounded-contexts
  - ai-agent
  - hitl
status: mvp-locked
updated: 2026-10-26
---

# 🧠 Nexus-Hub: Personal AI Switchboard & Multi-Channel Agent

> [!abstract] Định vị Sản phẩm (The Master Concept)
> **`nexus-hub`** là một **Personal AI Switchboard (Trạm điều phối AI cá nhân)** đứng chắn giữa bạn và các kênh công việc ồn ào bên ngoài (ClickUp, GitHub, Zalo, Messenger).
> - **Công thức cốt lõi:** `Matterbridge` *(Multi-channel Go routing)* + `HumanLayer` *(HITL Pending State)* + `Executive AI` *(Auto-Triage & Persona)* + `Zep` *(Rolling Summary & pgvector)* + `BYOI` *(Bring Your Own Supabase)*.
> - **Hạ tầng mục tiêu:** Chạy mượt trên `Oracle VM.Standard.E2.1.Micro` (1/8 OCPU, 1GB RAM) với mức tiêu thụ **~20-25MB RAM** nhờ kiến trúc Stateless Go Binary (Echo v4 + `pgx` + `sqlc`).

---

## 1. Kiến trúc 4 Miền Ngữ Cảnh (4 Bounded Contexts - DDD)

Hệ thống được chia thành **4 Bounded Contexts độc lập** trong `internal/` để đảm bảo không bị phụ thuộc chéo (`import cycle`) và dễ dàng mở rộng:

```mermaid
flowchart TB
    subgraph HTTP["🌐 Delivery Layer (internal/delivery/http - Echo v4)"]
        WH["Inbound Webhooks\n/webhook/:platform/:integration_id"]
        CH["Control Plane API / Webhook\n(Nhận lệnh từ Điện thoại / Web App)"]
        SH["Settings & Onboarding API\n(Cắm Supabase & Token động)"]
    end

    subgraph BC1["🔐 Context 1: TENANT & BYOI (internal/tenant)"]
        T_Svc["Tenant Resolver & sync.Map Cache"]
        T_Crypto["AES-256-GCM Credential Decryptor"]
        T_Pool["Dynamic Supabase Pool Manager\n(Lazy Connect + 5m Idle Eviction)"]
    end

    subgraph BC2["💬 Context 2: RELAY & THREADING (internal/relay)"]
        R_Filter["Zero-Token Pre-Filter\n(Chặn bot loop, icon rác)"]
        R_Debounce["Smart Debouncer (8s Timer)\nGom nhiều comment liên tiếp"]
        R_Map["Thread Mapper\n(External ID <-> Control Thread ID)"]
    end

    subgraph BC3["🗜️ Context 3: MEMORY & RAG (internal/memory)"]
        M_Build["Context Packet Builder (<500 tokens)\nSummary + 3 Last Msgs + Top 2 Vectors"]
        M_Comp["Background Rolling Compressor\n(Nén cuốn chiếu sau mỗi 5 tin)"]
    end

    subgraph BC4["🤖 Context 4: AGENTIC SWITCHBOARD (internal/agent)"]
        A_Orch["Agent Orchestrator"]
        A_Tier1["Tier 1: AI Router (Flash/Mini)\nChấm điểm Confidence & Triage"]
        A_Tier2["Tier 2: AI Actor (Sonnet/GPT-4o)\nReAct Loop (Reason -> Act -> Observe)"]
        A_HITL["Pending Approval State Machine\n(waiting_human -> approved/edited)"]
    end

    WH & CH & SH --> BC1
    BC1 -.->|"Cấp TenantContext & DB Pool"| BC2 & BC3 & BC4
    WH --> R_Filter --> R_Debounce --> A_Orch
    A_Orch <--> M_Build
    A_Orch --> A_Tier1
    A_Tier1 -- "Conf >= 85%\n(Auto-Pilot)" --> R_Map
    A_Tier1 -- "Conf < 85%\n(Co-Pilot)" --> A_HITL --> R_Map
    CH -->|"Khang ra lệnh / Duyệt"| A_Tier2 --> R_Map
    M_Comp -.->|"Chạy ngầm"| BC3
```

---

## 2. Cơ chế hoạt động của Bộ não AI: Routing vs. Agentic Loop

Hệ thống kết hợp **Model Routing (Chọn AI theo độ khó)** và **Agentic ReAct Loop (Vòng lặp tự chủ gọi hàm Go)**:

```mermaid
sequenceDiagram
    autonumber
    participant Relay as Context 2: Relay (Debouncer)
    participant Mem as Context 3: Memory Builder
    participant T1 as Tier 1: Router (Gemini Flash)
    participant T2 as Tier 2: Actor (Claude Sonnet)
    participant Tools as Go Tool Executor (GitHub/ClickUp)
    participant Human as Khang (Control Plane)

    Relay->>Mem: Sự kiện mới đã gom (sau 8s)
    Mem-->>T1: Context Packet (~450 tokens) + Cached System Prompt
    T1-->>T1: Phân tích & xuất Micro-JSON: {cat, urg, conf, draft, sum}

    alt Luồng Auto-Pilot (Confidence >= 85% - Câu hỏi tra cứu rõ ràng)
        T1->>Relay: Đăng `draft` kèm nhãn [🤖] lên ClickUp + Tag "ai-replied"
        T1->>Human: Báo cáo nhẹ (Silent Noti): "Đã tự trả lời @User..."
    else Luồng Co-Pilot / Cần quyết định (Confidence < 85%)
        T1->>Relay: Comment lên ClickUp: "🤖 Đã chuyển thông tin cho Khang kiểm tra..."
        T1->>Human: 🔔 Bắn Alert + Bản nháp + Nút bấm [Duyệt / Check CI / Sửa lệnh]
        
        Human->>T2: Nhắn lệnh: "Check xem PR nó pass lint/build chưa, rồi báo QC"
        
        loop Agentic ReAct Loop (Tối đa 5 bước)
            T2->>T2: 🧠 REASON: Cần kiểm tra trạng thái CI của PR
            T2->>Tools: 🛠️ ACT: Tool Call `check_github_ci(pr_url)`
            Tools-->>T2: 👀 OBSERVE: Trả về kết quả CI (Success / Failed Logs)
        end
        
        T2->>Relay: Thực thi `update_clickup_task()` & `post_comment()`
        T2->>Mem: Lưu quyết định mới vào `knowledge_vectors` (Học cho lần sau)
        T2-->>Human: ✅ Báo cáo hoàn tất công việc!
    end
```

---

## 3. Gia phả Kiến trúc (5 Open-Source Inspirations)

| Bounded Context trong `nexus-hub` | Kế thừa tinh hoa từ Repo | Cơ chế áp dụng vào Go |
| :--- | :--- | :--- |
| **`internal/relay`** | **`42wim/matterbridge`** & **`hatchet-dev`** | Chuẩn hóa mọi kênh về `NormalizedEvent`, ánh xạ `external_id <-> control_thread_id` và gom tin nhắn liên tiếp bằng `Debouncer` 8 giây. |
| **`internal/agent` (HITL)** | **`humanlayer/humanlayer`** | Máy trạng thái `PendingAction` (`waiting_human`): Dừng luồng chờ người thật duyệt hoặc gõ lệnh bổ sung từ điện thoại rồi mới thực thi tiếp. |
| **`internal/agent` (Triage)** | **`langchain-ai/executive-ai-assistant`** | Phân loại tín hiệu thành 3 nhóm (*Ignore / Auto-Reply / Escalate to Human*) và tự động trích xuất bài học từ câu trả lời của chủ nhân. |
| **`internal/memory`** | **`getzep/zep`** | Giữ chi phí token phẳng `O(1)` bằng cách nén mỗi 5 tin nhắn cũ thành `rolling_summary` + giữ 3 tin mới nhất + tra cứu `pgvector`. |

---

## 4. Thiết kế Database Schema cho MVP (`Supabase PostgreSQL + pgvector`)

```mermaid
erDiagram
    WORKSPACES ||--o{ INTEGRATIONS : configures
    WORKSPACES ||--o{ HUB_THREADS : owns
    WORKSPACES ||--o{ KNOWLEDGE_VECTORS : learns
    HUB_THREADS ||--o{ MESSAGE_LOGS : contains

    WORKSPACES {
        uuid id PK
        varchar email UK
        varchar display_name
        varchar api_key_hash "Xác thực nhanh cho App/CLI"
        text encrypted_db_url "Optional: Nếu user cắm Supabase riêng (BYOI)"
        text encrypted_llm_keys "AES-256: Key Gemini/OpenAI/Claude"
        jsonb ai_preferences "persona, mention_keywords, confidence_threshold (0.85)"
        timestamptz created_at
    }

    INTEGRATIONS {
        uuid id PK "Gắn lên URL: /webhook/:platform/:id"
        uuid workspace_id FK
        varchar role "source | control"
        varchar platform "clickup | github | zalo | messenger | webapp | discord | telegram"
        text encrypted_credentials "AES-256: API Token, Webhook Secret"
        jsonb config "external_user_id, target_channel_id..."
        boolean is_enabled
    }

    HUB_THREADS {
        uuid id PK
        uuid workspace_id FK
        uuid source_integration_id FK
        varchar source_platform
        varchar external_id "clickup_task_id / thread_id"
        varchar control_thread_id "ID luồng trên kênh điều khiển của bạn"
        varchar title
        varchar status "ai-checking | waiting-human | auto-replied | resolved"
        jsonb pending_action "Lưu bản nháp của Tier 1 đang chờ bạn duyệt"
        text rolling_summary "Tóm tắt cuốn chiếu ngữ cảnh"
        int unsummarized_count "Đếm >= 5 thì kích hoạt nén summary"
        jsonb metadata "pr_url, branch, custom_fields"
        timestamptz updated_at
    }

    MESSAGE_LOGS {
        bigserial id PK
        uuid thread_id FK
        varchar sender_role "external_user | human_owner | ai_router | ai_actor"
        varchar sender_name
        text content
        timestamptz created_at
    }

    KNOWLEDGE_VECTORS {
        uuid id PK
        uuid workspace_id FK
        uuid source_thread_id FK
        text topic_question
        text resolution
        vector_1536 embedding
        timestamptz created_at
    }
```

---

## 5. Ánh xạ Cấu trúc Code 4 Bounded Contexts (`Directory Mapping`)

```text
nexus-hub/
├── cmd/server/main.go                      # Entrypoint: Khởi tạo DI cho 4 Contexts & Echo Server
├── internal/
│   ├── tenant/                             # [CONTEXT 1: TENANT & BYOI]
│   │   ├── domain.go                       # Struct Workspace, Integration, AIPreferences
│   │   ├── service.go                      # Load & cache cấu hình theo integration_id
│   │   ├── pool_manager.go                 # Quản lý pgxpool động (tự đóng sau 5p idle)
│   │   └── crypto.go                       # Mã hóa / Giải mã AES-256-GCM
│   │
│   ├── relay/                              # [CONTEXT 2: RELAY & DEBOUNCER]
│   │   ├── domain.go                       # NormalizedEvent, SourcePort, ControlPlanePort
│   │   ├── debouncer.go                    # Gom tin nhắn liên tiếp trong 8 giây
│   │   └── router_service.go               # Điều phối gửi/nhận giữa SourcePort và ControlPlanePort
│   │
│   ├── memory/                             # [CONTEXT 3: MEMORY & TOKENOMICS]
│   │   ├── domain.go                       # ContextPacket, KnowledgeChunk
│   │   ├── builder.go                      # Ghép Rolling Summary + 3 tin cuối + Vector RAG
│   │   └── compressor.go                   # Background goroutine tóm tắt cuốn chiếu mỗi 5 tin
│   │
│   ├── agent/                              # [CONTEXT 4: AGENTIC SWITCHBOARD]
│   │   ├── domain.go                       # TriageDecision, PendingApproval, ToolCall
│   │   ├── orchestrator.go                 # Nối Memory -> Tier 1 -> HITL / Tier 2 -> Relay
│   │   ├── tier1_router.go                 # Gọi model rẻ (Flash/Mini) xuất JSON siêu gọn
│   │   ├── tier2_actor.go                  # Vòng lặp for ReAct gọi Tools ( tối đa 5 bước)
│   │   ├── prompts.go                      # System Prompts hỗ trợ Prompt Caching
│   │   └── tools.go                        # Khai báo Schema & thực thi hàm (ClickUp, GitHub CI)
│   │
│   ├── adapters/                           # [INFRASTRUCTURE PLUGINS]
│   │   ├── sources/{clickup,github,zalo,messenger}/
│   │   ├── controls/{webapp,discord,telegram}/
│   │   ├── llm/client.go
│   │   └── storage/                        # Code do sqlc generate
│   │
│   └── delivery/http/                      # [ECHO v4 HANDLERS]
│       ├── router.go
│       ├── webhook_handler.go              # Nhận sự kiện từ ClickUp/Zalo/GitHub
│       ├── control_handler.go              # Nhận lệnh duyệt/reply từ Điện thoại của bạn
│       ├── settings_handler.go             # API cấu hình BYOI & Persona
│       └── middleware.go                   # Verify HMAC & Supabase JWT / API Key
├── db/
│   ├── embed.go                            # //go:embed migrations/*.sql (Auto-migrate BYOI)
│   ├── migrations/
│   └── query/hub_queries.sql
└── ARCHITECTURE.md
```

---

## 6. Lộ trình Đóng Gói MVP (MVP Execution Plan - 5 Milestones)

> [!todo] Chỉ tập trung làm đúng 5 bước này để ra bản MVP chạy thực tế đầu tiên (Chưa làm Zalo/Messenger vội, tập trung ClickUp + GitHub + 1 Kênh Control trước)
> - [ ] **Milestone 1: Core Domain & Smart Debouncer (`internal/relay` + `Echo`)**
>   - Định nghĩa `NormalizedEvent`, `SourcePort`, `ControlPlanePort` trong `internal/relay/domain.go`.
>   - Viết `debouncer.go` (gom tin nhắn trong 8s bằng `time.Timer` + `sync.Mutex`) và test qua endpoint `/webhook/:platform/:id` trên Echo.
> - [ ] **Milestone 2: Database Schema & Memory Builder (`db/` + `internal/memory`)**
>   - Viết `00001_init_mvp.sql` + `hub_queries.sql` và chạy `sqlc generate` vào `internal/adapters/storage`.
>   - Viết `internal/memory/builder.go` lấy `rolling_summary` + 3 tin nhắn gần nhất.
> - [ ] **Milestone 3: Tier-1 AI Router & ClickUp Adapter (`internal/agent` + `adapters/sources/clickup`)**
>   - Viết ClickUp Client: Verify HMAC, tự thả reaction `👀` (`Acknowledge`), đăng comment kèm nhãn `[🤖]` và gắn tag.
>   - Viết `tier1_router.go` gọi Gemini Flash trả về JSON `{cat, conf, draft, sum}` và rẽ nhánh `conf >= 0.85`.
> - [ ] **Milestone 4: Control Plane & HITL Loop (`adapters/controls` + `tier2_actor.go`)**
>   - Cắm 1 kênh điều khiển đầu tiên trên điện thoại (Web/Mobile API hoặc Discord/Telegram Topic).
>   - Viết vòng lặp `for` trong `tier2_actor.go` kèm 2 Tool: `check_github_ci` và `update_clickup_task` để khi bạn gõ lệnh trên điện thoại, AI tự đi kiểm tra PR và báo cáo lên ClickUp.
> - [ ] **Milestone 5: BYOI & Deploy lên Oracle `E2.1.Micro` (`internal/tenant` + `scripts/build_oracle.sh`)**
>   - Bật mã hóa `AES-256-GCM` cho API Token và `embed.FS` auto-migration.
>   - Cross-compile ra binary Linux AMD64 (~15MB) và chạy qua `systemd` + Cloudflare Tunnel trên VPS Oracle.