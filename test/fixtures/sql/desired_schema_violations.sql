-- Negative fixture for make lint-sql (pin-glide negative fixture): the catalog
-- naming check, Schemalint and SQLFluff must each reject this file. It is
-- applied to the disposable database only, after the desired schemas passed.
-- The table is unnamed by convention, lacks comments and its keys and index
-- are misnamed; the lowercase keywords and the continuation indent are
-- deliberate style violations.
create table pin_glide.delivery_receivers (
    id bigint generated always as identity,
    constraint pk_delivery_receivers primary key (id)
);

create table pin_glide.receiversbad (
    id bigint generated always as identity,
    receiver_id bigint not null,
    constraint wrong_pk primary key (id),
    constraint wrong_fk foreign key (receiver_id)
        references pin_glide.delivery_receivers (id)
);

create index wrong_idx on pin_glide.receiversbad (receiver_id);
