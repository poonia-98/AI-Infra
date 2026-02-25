import sys
sys.path.insert(0, "/app")
import asyncio
from sqlalchemy import text
from database import engine, Base
import models

async def main():
    async with engine.begin() as conn:
        print("Creating UUID extension...")
        try:
            await conn.execute(text('CREATE EXTENSION IF NOT EXISTS "uuid-ossp"'))
            print("UUID extension created.")
        except Exception as e:
            print(f'extension creation skipped or failed: {e}')
        
        print("Creating all tables...")
        await conn.run_sync(Base.metadata.create_all)
        print("Tables created and committed.")
    
    await engine.dispose()
    print("Engine disposed. Done.")

if __name__ == '__main__':
    asyncio.run(main())
