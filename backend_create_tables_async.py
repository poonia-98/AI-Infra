import sys
sys.path.insert(0, "/app")
import asyncio
from sqlalchemy import text
from database import engine, Base

async def main():
    async with engine.begin() as conn:
        try:
            await conn.execute(text('CREATE EXTENSION IF NOT EXISTS "uuid-ossp"'))
        except Exception as e:
            print('extension creation skipped or failed:', e)
        await conn.run_sync(Base.metadata.create_all)
    await engine.dispose()
    print('tables created')

if __name__ == '__main__':
    asyncio.run(main())
