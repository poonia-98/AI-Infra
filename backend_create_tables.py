import sys
sys.path.insert(0, "/app")
from sqlalchemy import create_engine, text
from config import settings
from database import Base
import models

engine = create_engine(settings.SYNC_DATABASE_URL)
with engine.connect() as conn:
    try:
        conn.execute(text('CREATE EXTENSION IF NOT EXISTS "uuid-ossp"'))
        conn.commit()
    except Exception as e:
        print('extension creation skipped or failed:', e)

Base.metadata.create_all(engine)
print('tables created')
