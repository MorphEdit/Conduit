# Conduit

ตัวกลางซิงก์ Postgres ระหว่างหลายไซต์ (เช่น host บน cloud กับ local ในออฟฟิศ) ถ้าเน็ตหลุดหรือไซต์ใดล่ม
ทุกไซต์ยังทำงานต่อได้ แล้วข้อมูลจะตามกันเองเมื่อกลับมาเชื่อมต่อ

Conduit **ไม่ใช่ database** ข้อมูลจริงอยู่ใน Postgres ของแต่ละไซต์ แอปยังต่อ Postgres ตรงเหมือนเดิม
Conduit แค่คอยดูว่ามีอะไรเปลี่ยน แล้วส่งให้ไซต์อื่น

## สถานะ

| เฟส | เนื้อหา | สถานะ |
|---|---|---|
| 1 | ซิงก์ทางเดียว host → local, คิวถาวร, retry, ไม่ส่งซ้ำ | ✅ เสร็จ |
| 2 | ซิงก์สองทาง, กันซิงก์วน, จัดการ ID (คี่/คู่) | ⏳ |
| 3 | กฎ conflict รายตาราง, ตาราง host-only | ⏳ |
| 4 | ซิงก์ครั้งแรกสำหรับ node ใหม่ (snapshot) | ⏳ |
| 5 | ชุดทดสอบจำลองเหตุการณ์แบบครบ | ⏳ |

## ทำงานยังไง

```
[Postgres host]                                   [Postgres local]
   │ ① แอปเขียนข้อมูล                                    ▲
   ▼                                                    │ ⑤ เขียนลง DB ใน transaction เดียว
  WAL ─② capture อ่านผ่าน logical replication (pgoutput)    │   พร้อมบันทึก applied_seq
   ▼                                                    │
 ③ conduit.outbox (1 แถว = 1 transaction) ─④ HTTP POST ─► /v1/apply
   ▲                                                    │
   └──────────── ⑥ ack { applied: seq } ◄───────────────┘
                 ⑦ เลื่อน cursor แล้วลบแถวที่ peer ทุกตัวรับแล้ว
```

- **ไม่หาย:** capture ยืนยันตำแหน่ง slot หลังเขียน outbox สำเร็จแล้วเท่านั้น ถ้า Conduit ดับ Postgres จะเก็บ WAL รอไว้
- **ไม่เบิ้ล:** outbox ใช้ `lsn` เป็น unique และฝั่งรับเก็บ `applied_seq` ใน transaction เดียวกับข้อมูล ส่งซ้ำกี่รอบก็ข้าม
- **ไม่วน:** ฝั่งรับติด replication origin (`conduit_<node>`) และ capture ใช้ `origin 'none'` ข้อมูลที่รับมาจึงไม่ถูกส่งกลับ
- **ไม่ยิง trigger ซ้ำ:** ฝั่งรับใช้ `session_replication_role = replica` เหมือน apply worker ของ Postgres เอง
- **retry:** 1s → 2s → 4s … สูงสุด 30s

## โครงสร้าง

```
cmd/conduit/         main
internal/config/     โหลด YAML (+ ${ENV})
internal/capture/    อ่าน WAL → outbox
internal/store/      ตาราง conduit.outbox / peer_cursor / inbox_state
internal/sender/     ส่ง outbox ไปหา peer แต่ละตัว
internal/apply/      เขียน change ของ peer ลง DB
internal/server/     HTTP: /v1/apply, /health, /status
config/              config ของ test bench
testdata/init.sql    ตารางทดสอบ
scripts/demo.ps1     ทดสอบ end-to-end
```

## ทดลองรัน

ต้องมีแค่ Docker ไม่ต้องลง Go

```powershell
powershell -ExecutionPolicy Bypass -File scripts\demo.ps1 -Fresh
```

สคริปต์จะเปิด Postgres 2 ตัว + Conduit 2 ตัว แล้วทดสอบ:

1. insert / update / delete หลายชนิดข้อมูล (ไทย, array, jsonb, bytea, numeric, composite key)
2. ปิด conduit-local ระหว่างที่ host เขียนข้อมูล
3. ปิด Postgres ฝั่ง local
4. ปิด conduit-host (change รอใน replication slot)
5. ส่ง seq เก่าซ้ำ และ token ผิด

ดูสถานะ: <http://127.0.0.1:7420/status> (host), <http://127.0.0.1:7421/status> (local)

## Config

```yaml
node_id: host                 # a-z 0-9 _
listen: ":7420"
token: ${CONDUIT_TOKEN}       # ทุก node ต้องใช้ token เดียวกัน
database: postgres://user:pass@db:5432/app

capture:
  enabled: true
  slot: conduit_slot
  publication: conduit_pub
  schemas: [public]           # ห้ามใส่ conduit

peers:
  - id: local
    url: http://conduit-local:7420
```

## ข้อกำหนดของ Postgres

- `wal_level=logical` (ฝั่งที่เปิด capture)
- Postgres 16 ขึ้นไป (ใช้ `origin 'none'`)
- user ที่ Conduit ใช้ต้องเป็น superuser (สร้าง publication/slot/origin และตั้ง `session_replication_role`)
- ทุกตารางต้องมี primary key
- schema ต้องตรงกันทุก node — Conduit ซิงก์ข้อมูล ไม่ซิงก์ DDL และ `TRUNCATE`

## ข้อจำกัดตอนนี้ (เฟส 1)

- ซิงก์ทางเดียว; ถ้าเขียนที่ local ตอนนี้จะไม่ขึ้น host
- ยังไม่มีกฎ conflict — ฝั่งรับ upsert ทับด้วยข้อมูลของผู้ส่ง
- node ใหม่ต้องมีข้อมูลตั้งต้นเหมือนกันก่อน (ยังไม่มี snapshot)
- transaction ใหญ่มากจะถูกเก็บในหน่วยความจำก่อนเขียน outbox
- ยังส่งผ่าน HTTP ธรรมดา — ใช้งานจริงข้ามเน็ตต้องมี TLS (reverse proxy หรือ VPN) ก่อน
