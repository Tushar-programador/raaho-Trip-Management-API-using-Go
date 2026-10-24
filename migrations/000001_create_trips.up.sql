-- Trips: the core aggregate. Every business rule that can be expressed as a
-- constraint is enforced here, not only in application code.
CREATE TABLE trips (
    id           UUID         PRIMARY KEY,
    trip_number  VARCHAR(64)  NOT NULL,
    source       VARCHAR(255) NOT NULL,
    destination  VARCHAR(255) NOT NULL,
    driver_name  VARCHAR(100) NOT NULL,
    status       VARCHAR(20)  NOT NULL DEFAULT 'CREATED',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),

    CONSTRAINT trips_trip_number_key UNIQUE (trip_number),
    CONSTRAINT trips_status_check CHECK (
        status IN ('CREATED', 'ASSIGNED', 'IN_TRANSIT', 'COMPLETED', 'CANCELLED')
    ),
    CONSTRAINT trips_trip_number_not_blank CHECK (btrim(trip_number) <> ''),
    CONSTRAINT trips_source_not_blank      CHECK (btrim(source) <> ''),
    CONSTRAINT trips_destination_not_blank CHECK (btrim(destination) <> ''),
    CONSTRAINT trips_driver_name_not_blank CHECK (btrim(driver_name) <> ''),
    CONSTRAINT trips_updated_at_after_created_at CHECK (updated_at >= created_at)
);

-- Serves GET /trips?status=X ORDER BY created_at DESC, id DESC with an index
-- range scan and no sort step.
CREATE INDEX trips_status_created_at_id_idx ON trips (status, created_at DESC, id DESC);

-- Serves the unfiltered listing with the same ordering. The UNIQUE constraint
-- on trip_number already provides its own index, so none is added for it.
CREATE INDEX trips_created_at_id_idx ON trips (created_at DESC, id DESC);
