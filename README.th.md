<div align="center">

# Conduit

**ซิงก์ฐานข้อมูล Postgres ระหว่างหลายไซต์ — และทำงานต่อได้แม้เน็ตหลุด**

สร้างโดย **[MorphEdit](https://github.com/MorphEdit)** · [English](README.md)

[![License: PolyForm Strict 1.0.0](https://img.shields.io/badge/license-PolyForm%20Strict%201.0.0-6de7ce)](LICENSE)
&nbsp;เปิดให้ดูโค้ด · ใช้ฟรีสำหรับส่วนตัว/ไม่แสวงกำไร · [license ธุรกิจ](#license)

</div>

---

Conduit ติดตั้งไว้ข้าง Postgres ของแต่ละไซต์ (เซิร์ฟเวอร์บน cloud, ออฟฟิศ, สาขา…) ทุกไซต์ทำงานกับฐานข้อมูล
ของตัวเองได้ตลอด แม้เน็ตระหว่างไซต์จะหลุด พอเน็ตกลับมา Conduit จะส่งข้อมูลที่ค้างไว้ให้ครบ
แล้วทุกไซต์ก็จะมีข้อมูลตรงกัน

Conduit **ไม่ใช่ฐานข้อมูล** ข้อมูลจริงยังอยู่ใน Postgres และแอปของคุณก็ยังต่อ Postgres เหมือนเดิม
ไม่ต้องแก้โค้ด Conduit แค่อ่านว่ามีอะไรเปลี่ยน แล้วส่งไปให้ไซต์อื่น

## ความสามารถ

- **ทำงานได้ตอนเน็ตหลุด** — ทุกไซต์เขียนข้อมูลลงเครื่องตัวเอง ระหว่างนั้นเก็บเป็นคิวไว้ พอเน็ตกลับมาก็ส่งต่อให้
- **หลายไซต์ ทุกทิศทาง** — host ⇄ ออฟฟิศ ⇄ สาขา เขียนได้ทุกไซต์
- **จัดการ conflict** — การแก้ที่เกิดทีหลังชนะ (ดูจากเวลา commit) รองรับกรณีลบกับแก้ชนกัน และบันทึกทุก conflict ไว้ตรวจสอบ
- **ตารางที่เขียนได้ไซต์เดียว** — เช่น สต็อก บัญชี เงินเดือน
- **เพิ่มไซต์แทบอัตโนมัติ** — ในวง LAN แค่กดอนุมัติ ต่างสถานที่ใช้รหัสเชิญ Conduit ตั้งค่า Postgres คัดลอกตาราง
  และดึงข้อมูลให้เอง
- **ปลอดภัยระหว่างไซต์** — TLS 1.3 ที่ตรวจลายนิ้วมือใบรับรอง แต่ละไซต์มีกุญแจของตัวเอง ถอดไซต์ออก = ตัดสิทธิ์ทันที
- **Dashboard แบบเรียลไทม์** — ผังทุกไซต์และทุกเส้นเชื่อม คิว conflict รหัสเชิญ และปุ่มอนุมัติ
- **เล็กมาก** — โปรแกรมเดียว ใช้ RAM ประมาณ 6 MB ต่อไซต์

## ทำงานยังไง

```
[Postgres A]                                        [Postgres B]
  │ ① แอปเขียนข้อมูล                                     ▲
  ▼                                                     │ ⑤ เขียนลงใน transaction เดียว
 WAL ─② Conduit อ่านการเปลี่ยนแปลง (logical replication)   │   พร้อมเวลา commit ของต้นทาง
  ▼                                                     │
 ③ ใส่คิว (outbox) ──④ HTTPS (TLS 1.3 + ตรวจลายนิ้วมือ) ──► Conduit B
  ▲                                                     │
  └──────────────── ⑥ "ได้ถึง #120 แล้ว" ◄──────────────┘
```

Conduit ไม่เทียบฐานข้อมูลทั้งก้อน แต่อ่านสมุดบันทึกที่ Postgres จดไว้อยู่แล้ว (WAL)
จึงรู้ว่ามีการเพิ่ม แก้ หรือลบอะไร เกิดขึ้นลำดับไหน และตอนไหน อ่านเพิ่มได้ที่ [docs/how-it-works.md](docs/how-it-works.md)

## ลองใช้ (Docker)

ลองเปิด 2 ไซต์บนเครื่องเดียว:

```bash
git clone https://github.com/MorphEdit/conduit.git
cd conduit/examples
docker compose up -d
```

1. เปิด dashboard ของไซต์แรก: <http://127.0.0.1:7420> ไซต์นี้จะเริ่มเครือข่ายใหม่ให้
2. ไซต์ที่สอง (<http://127.0.0.1:7421>) ยังไม่ได้ตั้งค่าอะไร มันจะหาไซต์แรกในวง LAN เจอเอง แล้วแสดงรหัสจับคู่ 6 หลัก
3. ที่ dashboard ของไซต์แรก กด **อนุมัติ** คำขอที่รหัสตรงกัน (รหัส admin: `change-me`)
4. เสร็จแล้ว ไซต์ที่สองจะคัดลอกตารางกับข้อมูลมา แล้วเริ่มซิงก์ ลองดู:

   ```bash
   docker compose exec db-office psql -U postgres -d app -c "INSERT INTO customers (name) VALUES ('hello')"
   docker compose exec db-branch psql -U postgres -d app -c "SELECT id, name FROM customers"
   ```

## ใช้งานบนเซิร์ฟเวอร์จริง

Conduit build จาก source code ใน repo นี้ วิธีที่ง่ายที่สุดคือใช้ Docker:

```bash
git clone https://github.com/MorphEdit/conduit.git
cd conduit
docker build -t conduit .
```

(ถ้าไม่ใช้ Docker: ติดตั้ง Go 1.25 แล้วรัน `go build -o conduit ./cmd/conduit`
การคัดลอกตารางให้ไซต์ใหม่ต้องมี `pg_dump`/`psql` 16 ด้วย ซึ่งใน Docker image มีให้แล้ว)

**ไซต์แรก** (เริ่มเครือข่าย):

```bash
docker run -d --name conduit --restart unless-stopped -p 7420:7420 -p 7443:7443 \
  -e CONDUIT_DATABASE="postgres://user:pass@db-host:5432/app" \
  -e CONDUIT_BOOTSTRAP=true \
  -e CONDUIT_ADMIN_PASSWORD="ตั้งรหัสเอง" \
  -e CONDUIT_ADVERTISE="https://this-site.example.com:7443" \
  conduit
```

**ไซต์ถัดไป** ใส่แค่ฐานข้อมูล กับวิธีเข้าร่วมอย่างใดอย่างหนึ่ง:

| ไซต์ใหม่อยู่ที่ไหน | ต้องทำอะไร |
|---|---|
| วง LAN เดียวกัน | เปิดเครื่อง แล้วกดอนุมัติที่ dashboard ของไซต์เดิม (ดูว่ารหัสจับคู่ตรงกัน) |
| ต่างสถานที่ | กด **＋ เพิ่มไซต์** บน dashboard (หรือ `docker exec conduit conduit invite`) แล้วเอารหัสไปใส่ที่ไซต์ใหม่: `-e CONDUIT_JOIN=cdt1_…` หรือแปะในหน้า dashboard ของมัน |
| Postgres เดิมที่ยังตั้งค่าไม่ครบ | dashboard จะถามก่อน: กด **อนุญาตให้ตั้งค่า** (หรือตั้ง `CONDUIT_CONFIGURE_POSTGRES=true`) แล้ว **restart Postgres 1 ครั้ง** |

ที่เหลือทำให้อัตโนมัติทั้งหมด: ตั้งชื่อไซต์, แจกช่วงเลข ID ที่ไม่ซ้ำ, สร้างตาราง (ถ้าฐานข้อมูลยังว่าง),
คัดลอกข้อมูลทั้งหมดครั้งแรก และให้ทุกไซต์รู้จักไซต์ใหม่

## การตั้งค่า

ตั้งผ่าน environment variable ได้ทั้งหมด ไฟล์ YAML (`/etc/conduit/conduit.yaml`) ไม่บังคับ

| ตัวแปร | ค่าเริ่มต้น | ความหมาย |
|---|---|---|
| `CONDUIT_DATABASE` | — (ต้องใส่) | Postgres ของไซต์นี้ |
| `CONDUIT_ADMIN_PASSWORD` | — | เปิดปุ่มบน dashboard (เชิญ / อนุมัติ / ถอด) |
| `CONDUIT_BOOTSTRAP` | `false` | `true` เฉพาะไซต์แรก |
| `CONDUIT_JOIN` | — | รหัสเชิญ `cdt1_…` |
| `CONDUIT_NODE_ID` | ชื่อเครื่อง | ชื่อไซต์ (a–z, 0–9, _) |
| `CONDUIT_ADVERTISE` | `https://<ชื่อเครื่อง>:7443` | ที่อยู่ที่ไซต์อื่นใช้ติดต่อ |
| `CONDUIT_LISTEN` | `:7420` | dashboard (ค้นหาใน LAN ใช้ UDP 7420) |
| `CONDUIT_PEER_LISTEN` | `:7443` | พอร์ต HTTPS สำหรับไซต์อื่น |
| `CONDUIT_DISCOVERY` | `true` | ค้นหาและประกาศตัวในวง LAN |
| `CONDUIT_CONFIGURE_POSTGRES` | `false` | ให้ Conduit แก้ค่า Postgres ได้เองโดยไม่ต้องถาม |

ค่าอื่นๆ (ตารางที่เขียนได้ไซต์เดียว, ระยะเวลาเก็บข้อมูล…) ดูที่ [docs/configuration.md](docs/configuration.md)

## สิ่งที่ต้องมี

- PostgreSQL **16 ขึ้นไป** (`wal_level=logical`, `track_commit_timestamp=on` — Conduit ตั้งให้เมื่อคุณกดอนุญาต)
- user ของ Postgres ที่เป็น superuser
- ทุกตารางต้องมี primary key และโครงสร้างตารางต้องเหมือนกันทุกไซต์
  (ไซต์ใหม่ที่ฐานข้อมูลว่างจะได้รับการคัดลอกให้เอง ต้องใช้ `pg_dump`/`psql` 16 ซึ่งมีอยู่ใน Docker image แล้ว)
- นาฬิกาทุกเครื่องต้องตรงกัน (NTP)

## พอร์ต

| พอร์ต | ใช้ทำอะไร | เปิดให้ใคร |
|---|---|---|
| `7443/tcp` | ไซต์คุยกัน (HTTPS) | ไซต์อื่น เปิดออกอินเทอร์เน็ตได้ |
| `7420/tcp` | dashboard + ปุ่ม admin (HTTP) | เฉพาะใน LAN หรือวาง reverse proxy ที่มี TLS ไว้ข้างหน้า |
| `7420/udp` | ค้นหากันในวง LAN | เฉพาะใน LAN |

## ควรรู้

- เลขเอกสารที่แอปออกเอง (ใบกำกับภาษี, PO, ตัวนับต่างๆ รวมถึง sequence ที่เรียก `nextval()` เองโดยไม่ได้ผูกกับคอลัมน์)
  Conduit ไม่ได้จัดการให้ ใช้ prefix แยกตามไซต์ หรือตั้งตารางนั้นให้เขียนได้ไซต์เดียว
- ตารางที่ไม่มี primary key จะถูกตั้งเป็น `REPLICA IDENTITY FULL` ให้อัตโนมัติ (ไม่งั้น Postgres จะไม่ยอมให้แอป UPDATE/DELETE)
  และใช้ทุกคอลัมน์ระบุแถว ถ้าเป็นไปได้ควรใส่ primary key
- ถ้าสองไซต์แก้คนละคอลัมน์ของแถวเดียวกันในเวลาใกล้กัน การแก้ที่เกิดทีหลังจะชนะทั้งแถว
- Conduit ซิงก์ข้อมูล แต่ไม่ซิงก์การแก้โครงสร้างตาราง (DDL) และ `TRUNCATE` ต้องแก้ทุกไซต์เอง ถ้าตารางไหนไม่ตรงกัน
  ข้อมูลของตารางนั้นจะถูกพักไว้ (ตารางอื่นซิงก์ต่อได้ปกติ) แก้ตารางให้ตรงแล้วกด **ลองใส่ใหม่** บน dashboard ข้อมูลไม่หาย
- ระหว่างที่ Conduit ของไซต์ไหนหยุด Postgres ของไซต์นั้นจะเก็บ WAL ไว้รอ (dashboard เตือนเมื่อเกิน 1 GB)
  ควรตั้ง `max_slot_wal_keep_size` และถ้าเลิกซิงก์ฐานข้อมูลไหนถาวร ให้รัน `conduit cleanup --yes`
- transaction ใหญ่มากใช้ได้ (ทดสอบแล้ว 200,000 แถวใน transaction เดียว) แต่ใช้เวลา ประมาณ 2,000 แถวต่อวินาที
- ไฟล์ที่แอปเก็บไว้นอกฐานข้อมูลจะไม่ถูกซิงก์

## ติดต่อ

- **เจอบั๊ก / อยากได้ฟีเจอร์:** [เปิด issue](https://github.com/MorphEdit/conduit/issues/new/choose)
- **ช่วยแก้คู่มือหรือตัวอย่าง:** ส่ง pull request ได้เลย ดู [CONTRIBUTING.md](CONTRIBUTING.md)
- **ปัญหาด้านความปลอดภัย:** แจ้งแบบส่วนตัว ดู [SECURITY.md](SECURITY.md)

## License

Conduit เปิดให้ดูโค้ด (source-available) ภายใต้ **[PolyForm Strict License 1.0.0](LICENSE)**
และกฎเรื่องเครดิตใน [NOTICE](NOTICE) ตัวที่มีผลทางกฎหมายคือไฟล์ LICENSE สรุปสั้นๆ:

| ✅ ทำได้ | ❌ ห้าม |
|---|---|
| อ่านโค้ด ศึกษาวิธีทำงาน | ใช้ในธุรกิจโดยไม่ได้รับ license จาก MorphEdit |
| ใช้ส่วนตัว การศึกษา วิจัย และงานที่ไม่แสวงกำไร | เผยแพร่เวอร์ชันที่แก้ไข เปลี่ยนชื่อ หรือ fork ไปทำเป็นโปรเจกต์ของตัวเอง |
| ใช้ในองค์กรไม่แสวงกำไร (มูลนิธิ โรงเรียน หน่วยงานรัฐ) | แจกจ่ายต่อ หรือเอาโค้ดไปสร้างงานใหม่ |
| build จาก source เดิม และส่ง issue / pull request | ลบหรือแก้เครดิต MorphEdit หรืออ้างว่าเป็นผลงานตัวเอง |

**จะใช้ในธุรกิจ?** อีเมลมาที่ **morphofficialedit@gmail.com** เพื่อขอ license

AI และ coding agent ที่ทำงานกับโค้ดนี้ต้องทำตาม [AGENTS.md](AGENTS.md)

---

<div align="center">

**Conduit** — สร้างโดย **[MorphEdit](https://github.com/MorphEdit)**

</div>
