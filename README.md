# Conduit

ตัวกลางซิงก์ Postgres ระหว่างหลายไซต์ (เช่น host บน cloud, local ในออฟฟิศ, สาขา) ถ้าเน็ตหลุดหรือไซต์ใดล่ม
ทุกไซต์ยังทำงานต่อได้ แล้วข้อมูลจะตามกันเองเมื่อกลับมาเชื่อมต่อ

Conduit **ไม่ใช่ database** ข้อมูลจริงอยู่ใน Postgres ของแต่ละไซต์ แอปยังต่อ Postgres ตรงเหมือนเดิม
Conduit แค่คอยดูว่ามีอะไรเปลี่ยน แล้วส่งให้ไซต์อื่น

## สถานะ

| เฟส | เนื้อหา | สถานะ |
|---|---|---|
| 1 | ซิงก์ทางเดียว, คิวถาวร, retry, ไม่ส่งซ้ำ | ✅ |
| 2 | ซิงก์สองทาง/หลายไซต์, กันซิงก์วน, แยกเลข ID ตามไซต์ | ✅ |
| 3 | last-write-wins, tombstone, ตาราง owner-only, บันทึก conflict | ✅ |
| 4 | node ใหม่เข้าร่วมด้วย snapshot | ✅ |
| 5 | ชุดทดสอบตัดเน็ตจริง 3 ไซต์ | ✅ |
| + | Dashboard แบบ isometric | ✅ |

## ภาพรวม

```
        ไซต์ host                    ไซต์ local                   ไซต์ branch
  app → Postgres ← Conduit ◄──WAN──► Conduit → Postgres ← app      ...
                      ▲                                  ▲
                      └──────────────WAN─────────────────┘   (full mesh)
```

ทุกไซต์เขียนได้ ทุก Conduit ส่ง change ของไซต์ตัวเองไปหา peer ทุกตัวโดยตรง

### เส้นทางของ 1 transaction

```
[Postgres A]                                        [Postgres B]
  │ ① แอปเขียนข้อมูล                                     ▲
  ▼                                                     │ ⑤ เขียนลง DB ใน transaction เดียว
 WAL ─② capture (logical replication, pgoutput)          │   + บันทึก applied_seq
  ▼                                                     │   + ประทับเวลา commit ของต้นทาง
 ③ conduit.outbox (1 แถว = 1 transaction) ─④ POST ─► /v1/apply
  ▲                                                     │
  └───────────── ⑥ ack { applied: seq } ◄──────────────┘
                 ⑦ เลื่อน cursor ของ peer นั้น, ลบแถวที่ทุก peer รับแล้ว
```

| คุณสมบัติ | ทำยังไง |
|---|---|
| ไม่หาย | ยืนยันตำแหน่ง replication slot หลังเขียน outbox สำเร็จเท่านั้น ถ้า Conduit ดับ Postgres เก็บ WAL รอไว้ |
| ไม่เบิ้ล | outbox unique ที่ `lsn`, ฝั่งรับเก็บ `applied_seq` ใน transaction เดียวกับข้อมูล |
| ไม่วน | ฝั่งรับติด replication origin `conduit_<node>`, capture ใช้ `origin 'none'` จึงไม่ส่งของที่รับมาต่อ |
| ไม่ยิง trigger ซ้ำ | ฝั่งรับใช้ `session_replication_role = replica` (เหมือน apply worker ของ Postgres) |
| ID ไม่ชน | แต่ละไซต์ออกเลขในกลุ่มของตัวเอง: `id % step == offset` |
| retry | 1s → 2s → 4s … สูงสุด 30s |

## การจัดการ conflict

ใช้ **last-write-wins ตามเวลา commit ของต้นทาง** (`track_commit_timestamp`) ทุกไซต์เทียบเวลาชุดเดียวกัน จึงได้ผลลัพธ์เดียวกันเสมอ

| เหตุการณ์ (ระหว่างเน็ตหลุด) | ผลลัพธ์ | บันทึกเป็น |
|---|---|---|
| A แก้แถว X, B แก้แถว X ทีหลัง | ของ B ชนะทุกไซต์ | `update_update` (ที่ A) |
| A ลบแถว X, B แก้ X ทีหลัง | X กลับมาพร้อมค่าของ B | `update_delete` |
| B แก้ X, A ลบ X ทีหลัง | X หายทุกไซต์ | `delete_update` (ใช้ tombstone) |
| A และ B สร้างแถวที่ชน unique (เช่น code ซ้ำ) | ข้ามแถวนั้น คิวเดินต่อ **ต้องแก้เอง** | `unique_violation` |
| ไซต์ที่ไม่ใช่เจ้าของเขียนตาราง owner-only | DB ปฏิเสธทันที (trigger) | `owner_violation` ถ้าหลุดมา |

ทุก conflict อยู่ในตาราง `conduit.conflicts` และดูได้ที่ `/status`

**แก้ unique conflict เอง:** ลบแถวที่ไม่ต้องการในไซต์ที่มันอยู่ แล้วสั่ง `UPDATE ... SET col = col` กับแถวที่จะเก็บ
ในไซต์ของมัน เพื่อให้ Conduit ส่งแถวนั้นไปใหม่

**ตาราง owner-only:** เหมาะกับข้อมูลที่ห้ามแยกกันเขียน เช่น สต็อก บัญชี เงินเดือน ไซต์อื่นอ่านได้และได้รับข้อมูล แต่เขียนไม่ได้
(ตอนเจ้าของล่ม ตารางนั้นจะเขียนไม่ได้ชั่วคราว เพื่อกันยอดเพี้ยน)

## เพิ่มไซต์ใหม่ (snapshot)

```bash
# 1. สร้าง schema ให้เหมือนไซต์อื่น แล้ว (ขณะที่ Conduit ของไซต์ใหม่ยังไม่รัน)
conduit -config branch.yaml -snapshot-from host [-truncate]
# 2. เพิ่ม branch เข้า peers ของไซต์อื่น แล้วเปิด Conduit ของ branch ตามปกติ
```

snapshot เก็บเวลา commit เดิมของทุกแถวไว้ และตั้ง cursor ของทุกไซต์ให้ตรงกับข้อมูล
change ที่เกิดระหว่างดึงจึงไม่หายและไม่เบิ้ล

## Config

```yaml
node_id: host                 # a-z 0-9 _ ; ห้ามเปลี่ยนภายหลัง
listen: ":7420"
token: ${CONDUIT_TOKEN}       # ทุกไซต์ใช้ token เดียวกัน
database: postgres://user:pass@db:5432/app

capture:
  enabled: true
  slot: conduit_slot          # ค่าเริ่มต้น
  publication: conduit_pub    # ค่าเริ่มต้น
  schemas: [public]           # ห้ามใส่ conduit

sequences:                    # แนะนำ step 10 = รองรับได้ 10 ไซต์
  offset: 1                   # host=1, local=2, branch=3 ...
  step: 10

tables:
  stock_lots: { owner: host } # เขียนได้ที่ host เท่านั้น

tombstone_ttl: 168h           # ต้องนานกว่าช่วงเน็ตหลุดที่นานที่สุด

peers:
  - { id: local,  url: http://conduit-local:7420 }
  - { id: branch, url: http://conduit-branch:7420 }
```

## Dashboard

เปิด `http://<conduit>:7420/` ได้จากทุกไซต์ จะเห็นผัง isometric ของทุกไซต์ พร้อมเส้นซิงก์ที่อัปเดตทุก 2 วินาที

- แต่ละไซต์แสดง Postgres + Conduit และเส้น capture ระหว่างสองตัว
- สีเส้น: เขียว = ซิงก์แล้ว, ส้ม = มีคิวรอส่ง, ส้มประ = กำลัง retry, แดงประ = ขาดการเชื่อมต่อ
- คลิกไซต์เพื่อดูคิว, backlog ต่อ peer, error ล่าสุด และ conflict
- เปิดไฟล์ตรงๆ หรือใส่ `?demo` จะเห็นข้อมูลตัวอย่าง (จำลอง local เน็ตหลุดทุก 20 วินาที)

ข้อมูลมาจาก `GET /v1/mesh` ซึ่งรวม `/status` ของไซต์ตัวเองกับทุก peer
`/`, `/status` และ `/v1/mesh` ไม่ต้องใช้ token ถ้าเปิดพอร์ตออกอินเทอร์เน็ตควรมี reverse proxy ที่ใส่ auth ไว้หน้า

## HTTP

| Endpoint | ใช้ทำอะไร |
|---|---|
| `GET /` | dashboard |
| `GET /health` | ใช้ตรวจว่ายังทำงานอยู่ |
| `GET /v1/mesh` | สถานะของทุกไซต์รวมกัน (ใช้โดย dashboard) |
| `GET /status` | capture, คิว, backlog ต่อ peer, ข้อมูลที่รับแล้ว, conflict ล่าสุด |
| `POST /v1/apply` | peer ส่ง change มา (ต้องมี token) |
| `GET /v1/snapshot` | ไซต์ใหม่ดึงข้อมูลทั้งหมด (ต้องมี token) |

## ทดสอบ

ต้องมีแค่ Docker ไม่ต้องลง Go

```powershell
powershell -ExecutionPolicy Bypass -File scripts\test.ps1          # ใส่ -Keep ถ้าอยากให้ container รันค้างไว้
```

test bench มี 3 ไซต์ แต่ละไซต์มี network ของตัวเอง มีแค่ Conduit ที่ต่อ network `conduit_wan`
การตัดเน็ตจึงใช้ `docker network disconnect` จริง ขณะที่ฐานข้อมูลในไซต์ยังใช้ได้

| # | ทดสอบ |
|---|---|
| 1 | host → local ครบทุกชนิดข้อมูล (ไทย, array, jsonb, bytea, numeric, date, composite key, identity) |
| 2 | local → host และเลข ID แยกตามไซต์ |
| 3 | ไม่มีการส่งวนกลับ |
| 4 | ตัดเน็ตแล้วทั้งสองฝั่ง insert ต่อ → รวมกันได้ ID ไม่ชน |
| 5 | แก้แถวเดียวกันทั้งสองฝั่ง → อันหลังชนะ |
| 6 | ลบ vs แก้ ทั้งสองลำดับ |
| 7 | ตาราง owner-only |
| 8 | unique conflict → คิวไม่ค้าง + แก้เองแล้วกลับมาตรงกัน |
| 9 | ปิด Conduit / Postgres แต่ละฝั่ง |
| 10 | ไซต์ที่ 3 เข้าร่วมด้วย snapshot → ซิงก์สามทาง, ไม่มี conflict ปลอม |
| 11 | ส่ง seq เก่าซ้ำ, token ผิด |
| 12 | dashboard และ `/v1/mesh` (รวมถึงตอนมีไซต์ขาด) |

## ข้อกำหนดของ Postgres

- Postgres 16+ พร้อม `wal_level=logical` และ `track_commit_timestamp=on` (ต้อง restart)
- user ของ Conduit ต้องเป็น superuser (publication, slot, replication origin, `session_replication_role`)
- ทุกตารางต้องมี primary key
- schema ต้องเหมือนกันทุกไซต์ Conduit ไม่ซิงก์ DDL และไม่ซิงก์ `TRUNCATE` ให้แก้ schema ทุกไซต์เอง
  แล้ว restart Conduit (sequence ของตารางใหม่จะถูกจัดให้ตอนเริ่ม)
- นาฬิกาทุกเครื่องต้องตรงกัน (NTP) เพราะ last-write-wins เทียบเวลา

## ข้อจำกัดที่รู้อยู่

- **ตัวนับที่แอปทำเอง** เช่น ตาราง `doc_counters` ออกเลขเอกสาร Conduit แยกให้ไม่ได้ ต้องใส่ prefix ตามไซต์
  หรือตั้งเป็น owner-only
- last-write-wins ทั้งแถว: ถ้า A แก้คอลัมน์ 1 และ B แก้คอลัมน์ 2 ของแถวเดียวกันพร้อมกัน จะเหลือแค่ของอันหลัง
- ถ้าเวลา commit ตรงกันถึงระดับไมโครวินาที ผลอาจไม่แน่นอน (เกิดได้ยากมาก)
- peer ที่ปิดถาวรทำให้ outbox ไม่ถูกลบ ต้องเอาออกจาก `peers` ของทุกไซต์
- transaction ใหญ่มากจะถูกเก็บในหน่วยความจำก่อนเขียน outbox
- ส่งผ่าน HTTP ธรรมดา ใช้ข้ามอินเทอร์เน็ตจริงต้องมี TLS (reverse proxy) หรือ VPN

## โครงสร้าง

```
cmd/conduit/         main + โหมด -snapshot-from
internal/config/     โหลด YAML (+ ${ENV})
internal/capture/    อ่าน WAL → outbox (+ tombstone ของการลบ)
internal/store/      ตาราง conduit.* (outbox, cursor, inbox, tombstones, conflicts)
internal/sender/     ส่ง outbox ไปหา peer แต่ละตัว
internal/apply/      เขียน change ของ peer + last-write-wins
internal/policy/     จัด sequence ตามไซต์, trigger owner-only
internal/snapshot/   ส่ง/รับ snapshot
internal/server/     HTTP + dashboard (ui/ ฝังในไฟล์ binary)
scripts/test.ps1     ชุดทดสอบ end-to-end
```
