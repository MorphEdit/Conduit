# Contributing to Conduit

Thanks for helping! Conduit is developed by [MorphEdit](https://github.com/MorphEdit) and is
source-available under the PolyForm Strict License 1.0.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)).

## Pull requests

Welcome for code fixes, documentation (English or Thai), examples and translations.

- The [NOTICE](NOTICE) lets you change the code **only to prepare a pull request to this repository**
  (and keep a GitHub fork for that). Please don't publish modified versions anywhere else.
- Keep every copyright header and the MorphEdit credit intact; start new files with the same header.
- Keep pull requests small and focused, and describe what you changed and why.
- Run the tests if you touched code: `powershell -ExecutionPolicy Bypass -File scripts\test.ps1`
  (needs Docker).
- By opening a pull request you agree that MorphEdit may use, change and distribute your contribution
  as part of Conduit under any terms (see NOTICE).

## Reporting bugs and asking for features

Open an [issue](https://github.com/MorphEdit/conduit/issues/new/choose) and use the template.
The most useful bug reports include:

- `conduit version` output
- PostgreSQL version and how you run Conduit (Docker, built from source, OS)
- What you did, what you expected, what happened
- The relevant lines of the Conduit log (remove passwords and invite codes first)
- A screenshot of the dashboard if it helps

## Security issues

Please do **not** open a public issue — see [SECURITY.md](SECURITY.md).

---

# การมีส่วนร่วม (ภาษาไทย)

ขอบคุณที่ช่วยครับ Conduit พัฒนาโดย [MorphEdit](https://github.com/MorphEdit) เปิดให้ดูโค้ดภายใต้ PolyForm Strict License 1.0.0

- **ส่ง pull request ได้:** แก้บั๊ก คู่มือ (ไทย/อังกฤษ) ตัวอย่าง คำแปล — แก้โค้ดได้**เฉพาะเพื่อส่ง PR มาที่ repo นี้**
  ห้ามเอาเวอร์ชันที่แก้ไปเผยแพร่ที่อื่น และต้องเก็บหัวลิขสิทธิ์กับเครดิต MorphEdit ไว้ครบ
- **จะใช้ในธุรกิจ:** อีเมล morphofficialedit@gmail.com
- **เจอบั๊กหรืออยากได้ฟีเจอร์:** เปิด [issue](https://github.com/MorphEdit/conduit/issues/new/choose) ตามแบบฟอร์ม
  แนบผลของ `conduit version` เวอร์ชัน Postgres และ log ที่เกี่ยวข้อง (ลบรหัสผ่านกับรหัสเชิญออกก่อน)
- **ปัญหาด้านความปลอดภัย:** อย่าเปิด issue สาธารณะ ดู [SECURITY.md](SECURITY.md)
